package tenantstore

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/valyala/fasthttp"
)

// StaticRegistry resolves a single store for all tenants. Intended for unit tests.
type StaticRegistry struct {
	Store    configstore.ConfigStore
	TenantID string
}

// NewStaticRegistry returns a resolver backed by one ConfigStore.
func NewStaticRegistry(store configstore.ConfigStore, tenantID string) *StaticRegistry {
	if tenantID == "" {
		tenantID = "__test_tenant__"
	}
	return &StaticRegistry{Store: store, TenantID: tenantID}
}

func (r *StaticRegistry) GetStoreFromContext(ctx context.Context) configstore.ConfigStore {
	if r == nil || r.Store == nil {
		return nil
	}
	if tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string); tenantID != "" && tenantID != r.TenantID {
		return nil
	}
	return r.Store
}

func (r *StaticRegistry) GetStoreFromRequestCtx(ctx *fasthttp.RequestCtx) configstore.ConfigStore {
	if r == nil || r.Store == nil || ctx == nil {
		return nil
	}
	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	if tenantID != "" && tenantID != r.TenantID {
		return nil
	}
	return r.Store
}

func (r *StaticRegistry) RequireStoreFromRequestCtx(ctx *fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	store := r.GetStoreFromRequestCtx(ctx)
	if store == nil {
		return nil, fmt.Errorf("tenant context required")
	}
	return store, nil
}

func (r *StaticRegistry) ListTenantIDs(_ context.Context) []string {
	if r == nil || r.Store == nil {
		return nil
	}
	return []string{r.TenantID}
}

func (r *StaticRegistry) SyncTenants(context.Context) error { return nil }

func (r *StaticRegistry) GetStoreForTenant(_ context.Context, tenantID string) configstore.ConfigStore {
	if r == nil || r.Store == nil || tenantID != r.TenantID {
		return nil
	}
	return r.Store
}

func (r *StaticRegistry) PeekStoreForTenant(tenantID string) configstore.ConfigStore {
	return r.GetStoreForTenant(context.Background(), tenantID)
}
