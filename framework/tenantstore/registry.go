package tenantstore

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/valyala/fasthttp"
)

// Resolver resolves per-tenant ConfigStore instances from request or context.
type Resolver interface {
	GetStoreFromContext(ctx context.Context) configstore.ConfigStore
	GetStoreFromRequestCtx(ctx *fasthttp.RequestCtx) configstore.ConfigStore
	RequireStoreFromRequestCtx(ctx *fasthttp.RequestCtx) (configstore.ConfigStore, error)
	ListTenantIDs(ctx context.Context) []string
	SyncTenants(ctx context.Context) error
	GetStoreForTenant(ctx context.Context, tenantID string) configstore.ConfigStore
	// PeekStoreForTenant returns an already-open store without creating a pool
	// or refreshing lastUsed. Use this from background workers.
	PeekStoreForTenant(tenantID string) configstore.ConfigStore
}

// TenantConfigRegistry resolves per-tenant ConfigStore instances from request
// or context. Every read/write path must carry BifrostContextKeyTenantID (set by
// TenantMiddleware on authenticated routes).
type TenantConfigRegistry struct {
	manager *TenantDBManager
}

// NewTenantConfigRegistry creates a registry backed by the given TenantDBManager.
func NewTenantConfigRegistry(manager *TenantDBManager) *TenantConfigRegistry {
	return &TenantConfigRegistry{
		manager: manager,
	}
}

// GetStoreFromContext returns the ConfigStore for the tenant identified by
// BifrostContextKeyTenantID in ctx. Returns nil when tenantID is absent or the
// tenant is not registered.
func (r *TenantConfigRegistry) GetStoreFromContext(ctx context.Context) configstore.ConfigStore {
	if r == nil || r.manager == nil {
		return nil
	}
	tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		return nil
	}
	store, err := r.manager.GetStore(ctx, tenantID)
	if err != nil || store == nil {
		return nil
	}
	return store
}

// GetStoreFromRequestCtx reads the tenant ID from fasthttp user values (set by
// TenantMiddleware) and returns the matching ConfigStore.
func (r *TenantConfigRegistry) GetStoreFromRequestCtx(ctx *fasthttp.RequestCtx) configstore.ConfigStore {
	if r == nil || ctx == nil {
		return nil
	}
	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		return nil
	}
	store, err := r.manager.GetStore(ctx, tenantID)
	if err != nil || store == nil {
		return nil
	}
	return store
}

// RequireStoreFromRequestCtx returns the tenant ConfigStore or an error when the
// request has no tenant context.
func (r *TenantConfigRegistry) RequireStoreFromRequestCtx(ctx *fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	store := r.GetStoreFromRequestCtx(ctx)
	if store == nil {
		return nil, fmt.Errorf("tenant context required")
	}
	return store, nil
}

// ListTenantIDs returns the tenant IDs currently registered with the manager.
func (r *TenantConfigRegistry) ListTenantIDs(_ context.Context) []string {
	if r == nil || r.manager == nil {
		return nil
	}
	return r.manager.ListTenantIDs()
}

// SyncTenants closes pools for tenants that were soft-deleted in the global DB.
func (r *TenantConfigRegistry) SyncTenants(ctx context.Context) error {
	if r == nil || r.manager == nil {
		return nil
	}
	return r.manager.SyncTenantsFromGlobalDB(ctx)
}

// GetStoreForTenant returns the ConfigStore for a specific tenant ID.
func (r *TenantConfigRegistry) GetStoreForTenant(ctx context.Context, tenantID string) configstore.ConfigStore {
	if r == nil || r.manager == nil || tenantID == "" {
		return nil
	}
	store, err := r.manager.GetStore(ctx, tenantID)
	if err != nil || store == nil {
		return nil
	}
	return store
}

// PeekStoreForTenant returns the open ConfigStore for tenantID without opening
// a pool or refreshing lastUsed.
func (r *TenantConfigRegistry) PeekStoreForTenant(tenantID string) configstore.ConfigStore {
	if r == nil || r.manager == nil {
		return nil
	}
	return r.manager.PeekStore(tenantID)
}
