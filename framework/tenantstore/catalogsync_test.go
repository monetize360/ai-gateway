package tenantstore

import (
	"context"
	"errors"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

type noopLogger struct{}

func (noopLogger) Debug(string, ...any)                      {}
func (noopLogger) Info(string, ...any)                       {}
func (noopLogger) Warn(string, ...any)                       {}
func (noopLogger) Error(string, ...any)                      {}
func (noopLogger) Fatal(string, ...any)                      {}
func (noopLogger) SetLevel(schemas.LogLevel)                 {}
func (noopLogger) SetOutputType(schemas.LoggerOutputType)    {}
func (noopLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

type catalogSyncSourceStore struct {
	pricing []configstoreTables.TableModelPricing
	params  []configstoreTables.TableModelParameters
}

func (s *catalogSyncSourceStore) GetModelPrices(context.Context) ([]configstoreTables.TableModelPricing, error) {
	return s.pricing, nil
}

func (s *catalogSyncSourceStore) GetModelParameters(context.Context) ([]configstoreTables.TableModelParameters, error) {
	return s.params, nil
}

type catalogSyncTargetStore struct {
	pricingUpserts []configstoreTables.TableModelPricing
	paramsUpserts  []configstoreTables.TableModelParameters
}

func (t *catalogSyncTargetStore) ExecuteTransaction(_ context.Context, fn func(tx *gorm.DB) error) error {
	return fn(nil)
}

func (t *catalogSyncTargetStore) UpsertModelPrices(_ context.Context, pricing *configstoreTables.TableModelPricing, _ ...*gorm.DB) error {
	if pricing == nil {
		return errors.New("nil pricing")
	}
	t.pricingUpserts = append(t.pricingUpserts, *pricing)
	return nil
}

func (t *catalogSyncTargetStore) UpsertModelParameters(_ context.Context, params *configstoreTables.TableModelParameters, _ ...*gorm.DB) error {
	if params == nil {
		return errors.New("nil params")
	}
	t.paramsUpserts = append(t.paramsUpserts, *params)
	return nil
}

func TestReplicateModelCatalogDataCopiesPricingAndParameters(t *testing.T) {
	source := &catalogSyncSourceStore{
		pricing: []configstoreTables.TableModelPricing{
			{Model: "gpt-4o", Provider: "openai", Mode: "chat"},
		},
		params: []configstoreTables.TableModelParameters{
			{Model: "gpt-4o", Data: `{"max_output_tokens":8192}`},
		},
	}
	target := &catalogSyncTargetStore{}

	if err := ReplicateModelCatalogData(context.Background(), source, target, noopLogger{}); err != nil {
		t.Fatalf("ReplicateModelCatalogData failed: %v", err)
	}
	if len(target.pricingUpserts) != 1 {
		t.Fatalf("expected 1 pricing upsert, got %d", len(target.pricingUpserts))
	}
	if len(target.paramsUpserts) != 1 {
		t.Fatalf("expected 1 parameters upsert, got %d", len(target.paramsUpserts))
	}
	if target.pricingUpserts[0].Model != "gpt-4o" {
		t.Fatalf("unexpected pricing model: %s", target.pricingUpserts[0].Model)
	}
	if target.paramsUpserts[0].Data != `{"max_output_tokens":8192}` {
		t.Fatalf("unexpected params payload: %s", target.paramsUpserts[0].Data)
	}
}

func TestReplicateModelCatalogDataSkipsWhenSourceEmpty(t *testing.T) {
	source := &catalogSyncSourceStore{}
	target := &catalogSyncTargetStore{}

	if err := ReplicateModelCatalogData(context.Background(), source, target, noopLogger{}); err != nil {
		t.Fatalf("expected nil error for empty source, got %v", err)
	}
	if len(target.pricingUpserts) != 0 || len(target.paramsUpserts) != 0 {
		t.Fatalf("expected no upserts for empty source")
	}
}
