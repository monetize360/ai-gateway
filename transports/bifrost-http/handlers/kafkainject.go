package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/tenantstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib/kafkainject"
	"github.com/valyala/fasthttp"
)

// KafkaIngestHandler serves high-throughput Kafka ingest for usage events.
type KafkaIngestHandler struct {
	registry    tenantstore.Resolver
	pool        *kafkainject.Pool
	schemaCache *kafkainject.SchemaCache
}

// NewKafkaIngestHandler creates a Kafka ingest handler with its own producer pool.
func NewKafkaIngestHandler(registry tenantstore.Resolver) *KafkaIngestHandler {
	return NewKafkaIngestHandlerWithPool(registry, kafkainject.NewPool(registry), kafkainject.NewSchemaCache(registry))
}

// NewKafkaIngestHandlerWithPool creates a Kafka ingest handler that shares a producer pool.
func NewKafkaIngestHandlerWithPool(registry tenantstore.Resolver, pool *kafkainject.Pool, schemaCache *kafkainject.SchemaCache) *KafkaIngestHandler {
	if pool == nil {
		pool = kafkainject.NewPool(registry)
	}
	if schemaCache == nil {
		schemaCache = kafkainject.NewSchemaCache(registry)
	}
	return &KafkaIngestHandler{
		registry:    registry,
		pool:        pool,
		schemaCache: schemaCache,
	}
}

// UsagePublisher returns an InferenceUsage publisher backed by this handler's pool.
func (h *KafkaIngestHandler) UsagePublisher() *kafkainject.UsagePublisher {
	if h == nil {
		return nil
	}
	return kafkainject.NewUsagePublisher(h.pool)
}

// RegisterRoutes registers the standard usage endpoint and the deprecated
// connection/datasource-based Kafka endpoint.
func (h *KafkaIngestHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.POST("/v1/ingest/usage", lib.ChainMiddlewares(h.ingestUsage, middlewares...))
	// Deprecated: use POST /v1/ingest/usage. The legacy endpoint remains
	// available for clients that still depend on integration configuration.
	r.POST("/v1/ingest/kafka", lib.ChainMiddlewares(h.ingest, middlewares...))
}

// Close releases cached Kafka producers.
func (h *KafkaIngestHandler) Close() {
	if h.pool != nil {
		h.pool.Close()
	}
}

type kafkaIngestRequest struct {
	ConnectionID string          `json:"connectionId"`
	DataSourceID string          `json:"dataSourceId"`
	Key          string          `json:"key"`
	Message      json.RawMessage `json:"message"`
}

type usageIngestRequest struct {
	Key     string          `json:"key"`
	Message json.RawMessage `json:"message"`
}

type kafkaIngestResponse struct {
	Status    string `json:"status"`
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
}

func (h *KafkaIngestHandler) ingest(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Deprecation", "true")
	ctx.Response.Header.Set("Link", `</v1/ingest/usage>; rel="successor-version"`)

	body := ctx.PostBody()
	if len(body) > kafkainject.MaxMessageBytes {
		SendError(ctx, fasthttp.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		SendError(ctx, fasthttp.StatusUnauthorized, "tenant context missing")
		return
	}

	var req kafkaIngestRequest
	if err := sonic.Unmarshal(body, &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Message) == 0 || string(req.Message) == "null" {
		SendError(ctx, fasthttp.StatusBadRequest, "message is required")
		return
	}

	connectionID := strings.TrimSpace(req.ConnectionID)
	if connectionID == "" {
		connectionID = kafkainject.DefaultConnectionID
	}
	dataSourceID := strings.TrimSpace(req.DataSourceID)
	if dataSourceID == "" {
		dataSourceID = kafkainject.DefaultDataSourceID
	}

	reqCtx, cancel := context.WithTimeout(ctx, kafkainject.ProduceTimeout+2*time.Second)
	defer cancel()

	entry, err := h.pool.GetOrCreate(reqCtx, tenantID, connectionID, dataSourceID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	fields, err := h.schemaCache.GetOrLoad(reqCtx, tenantID, entry.MObjectID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	var message map[string]any
	if err := sonic.Unmarshal(req.Message, &message); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "message must be a JSON object")
		return
	}
	if err := kafkainject.ValidateMessage(message, fields); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	h.publish(ctx, reqCtx, entry, req.Key, req.Message)
}

func (h *KafkaIngestHandler) ingestUsage(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) > kafkainject.MaxMessageBytes {
		SendError(ctx, fasthttp.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		SendError(ctx, fasthttp.StatusUnauthorized, "tenant context missing")
		return
	}

	var req usageIngestRequest
	if err := sonic.Unmarshal(body, &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Message) == 0 || string(req.Message) == "null" {
		SendError(ctx, fasthttp.StatusBadRequest, "message is required")
		return
	}
	var message map[string]any
	if err := sonic.Unmarshal(req.Message, &message); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "message must be a JSON object")
		return
	}

	payload, err := ensureOrganizationID(ctx, message)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, kafkainject.ProduceTimeout+2*time.Second)
	defer cancel()

	entry, err := h.pool.GetOrCreateStandard(tenantID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	h.publish(ctx, reqCtx, entry, req.Key, payload)
}

func ensureOrganizationID(ctx *fasthttp.RequestCtx, message map[string]any) ([]byte, error) {
	if organizationIDFromMessage(message) == "" {
		morgID, _ := ctx.UserValue(ingestContextKeyMorgID).(string)
		morgID = strings.TrimSpace(morgID)
		if morgID == "" {
			return nil, fmt.Errorf("organizationId is required in the message or as morgId in the token")
		}
		message["organizationId"] = morgID
	}
	payload, err := sonic.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize usage message: %w", err)
	}
	return payload, nil
}

func organizationIDFromMessage(message map[string]any) string {
	for _, key := range []string{"organizationId", "organization_id"} {
		value, ok := message[key]
		if !ok || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text != "" {
			return text
		}
	}
	return ""
}

func (h *KafkaIngestHandler) publish(ctx *fasthttp.RequestCtx, reqCtx context.Context, entry *kafkainject.ProducerEntry, key string, message []byte) {
	topic, partition, offset, err := kafkainject.ProduceSync(reqCtx, entry, key, message)
	if err != nil {
		if reqCtx.Err() != nil {
			SendError(ctx, fasthttp.StatusGatewayTimeout, fmt.Sprintf("timed out publishing kafka message: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusBadGateway, fmt.Sprintf("failed to publish kafka message: %v", err))
		return
	}

	SendJSON(ctx, kafkaIngestResponse{
		Status:    "published",
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
	})
}
