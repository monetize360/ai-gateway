package tenantstore

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

// TenantConfigRegistry bridges TenantDBManager into context-aware ConfigStore
// resolution. Call GetStoreFromContext(ctx) wherever a ConfigStore is needed;
// it returns the tenant-specific store when the context carries a
// BifrostContextKeyTenantID value, or falls back to the default store for
// requests that have no tenant context (e.g. admin / health-check routes).
type TenantConfigRegistry struct {
	manager      *TenantDBManager
	defaultStore configstore.ConfigStore
}

// NewTenantConfigRegistry creates a registry backed by the given TenantDBManager.
// defaultStore is used when no tenantID is present in the context; it is
// typically the shared single-tenant ConfigStore loaded from config.json.
func NewTenantConfigRegistry(manager *TenantDBManager, defaultStore configstore.ConfigStore) *TenantConfigRegistry {
	return &TenantConfigRegistry{
		manager:      manager,
		defaultStore: defaultStore,
	}
}

// GetStoreFromContext returns the ConfigStore for the tenant identified by
// BifrostContextKeyTenantID in ctx. Falls back to the defaultStore when:
//   - there is no tenantID in the context
//   - manager fails to load the tenant store (error is logged, not propagated)
func (r *TenantConfigRegistry) GetStoreFromContext(ctx context.Context) configstore.ConfigStore {
	tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	if tenantID == "" {
		return r.defaultStore
	}

	store, err := r.manager.GetStore(ctx, tenantID)
	if err != nil || store == nil {
		return r.defaultStore
	}
	return store
}

// DefaultStore returns the fallback ConfigStore (non-tenant path).
func (r *TenantConfigRegistry) DefaultStore() configstore.ConfigStore {
	return r.defaultStore
}
