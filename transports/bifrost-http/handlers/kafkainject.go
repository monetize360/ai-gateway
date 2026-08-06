package handlers

import (
	"context"
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
	return kafkainject.NewUsagePublisher(h.pool, h.schemaCache)
}

// RegisterRoutes registers POST /v1/ingest/kafka.
func (h *KafkaIngestHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.POST("/v1/ingest/kafka", lib.ChainMiddlewares(h.ingest, middlewares...))
}

// Close releases cached Kafka producers.
func (h *KafkaIngestHandler) Close() {
	if h.pool != nil {
		h.pool.Close()
	}
}

type kafkaIngestRequest struct {
	ConnectionID string         `json:"connectionId"`
	DataSourceID string         `json:"dataSourceId"`
	Key          string         `json:"key"`
	Message      map[string]any `json:"message"`
}

type kafkaIngestResponse struct {
	Status    string `json:"status"`
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
}

func (h *KafkaIngestHandler) ingest(ctx *fasthttp.RequestCtx) {
	if len(ctx.PostBody()) > kafkainject.MaxMessageBytes {
		SendError(ctx, fasthttp.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		SendError(ctx, fasthttp.StatusUnauthorized, "tenant context missing")
		return
	}

	var req kafkaIngestRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Message == nil {
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
	if err := kafkainject.ValidateMessage(req.Message, fields); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	payload, err := sonic.Marshal(req.Message)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "failed to serialize message")
		return
	}

	topic, partition, offset, err := kafkainject.ProduceSync(reqCtx, entry, req.Key, payload)
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
