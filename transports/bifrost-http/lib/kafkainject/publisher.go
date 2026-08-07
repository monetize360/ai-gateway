package kafkainject

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// UsagePublisher publishes InferenceUsage messages via the shared Kafka producer pool.
// Method signature matches governance.UsageEventPublisher without importing that package.
type UsagePublisher struct {
	pool         *Pool
	schemaCache  *SchemaCache
	connectionID string
	dataSourceID string
}

// NewUsagePublisher creates a publisher bound to the standard InferenceUsage connection/datasource.
func NewUsagePublisher(pool *Pool, schemaCache *SchemaCache) *UsagePublisher {
	return &UsagePublisher{
		pool:         pool,
		schemaCache:  schemaCache,
		connectionID: DefaultConnectionID,
		dataSourceID: DefaultDataSourceID,
	}
}

// PublishUsage validates and produces an InferenceUsage Kafka message for the tenant.
func (p *UsagePublisher) PublishUsage(ctx context.Context, tenantID, key string, message map[string]any) error {
	if p == nil || p.pool == nil {
		return fmt.Errorf("kafka usage publisher not configured")
	}
	if strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("tenantID is required")
	}
	if message == nil {
		return fmt.Errorf("message is required")
	}

	publishCtx, cancel := context.WithTimeout(ctx, ProduceTimeout+2*time.Second)
	defer cancel()

	entry, err := p.pool.GetOrCreate(publishCtx, tenantID, p.connectionID, p.dataSourceID)
	if err != nil {
		return err
	}

	if p.schemaCache != nil {
		fields, err := p.schemaCache.GetOrLoad(publishCtx, tenantID, entry.MObjectID)
		if err != nil {
			return err
		}
		if err := ValidateMessage(message, fields); err != nil {
			return err
		}
	}

	payload, err := sonic.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to serialize usage message: %w", err)
	}
	if len(payload) > MaxMessageBytes {
		return fmt.Errorf("usage message exceeds max size")
	}

	_, _, _, err = ProduceSync(publishCtx, entry, key, payload)
	return err
}
