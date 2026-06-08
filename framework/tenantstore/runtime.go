package tenantstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

// TenantDBManager holds a pre-loaded map of configstore.ConfigStore keyed by tenantID.
// Stores are initialised eagerly at startup via LoadAll; after that every request
// hits only a read lock for O(1) access.
type TenantDBManager struct {
	mu           sync.RWMutex
	stores       map[string]configstore.ConfigStore // tenantID → ConfigStore (populated at startup)
	globalDB     *GlobalDB
	poolSettings configstore.PostgresPoolSettings
	logger       schemas.Logger
}

// NewTenantDBManager creates a TenantDBManager backed by the supplied GlobalDB.
func NewTenantDBManager(globalDB *GlobalDB, pool configstore.PostgresPoolSettings, logger schemas.Logger) *TenantDBManager {
	return &TenantDBManager{
		stores:       make(map[string]configstore.ConfigStore),
		globalDB:     globalDB,
		poolSettings: pool,
		logger:       logger,
	}
}

// LoadAll queries the tenants table and opens a ConfigStore for every tenant.
// It must be called once at server startup before serving requests.
// Tenants that fail to connect are logged and skipped (non-fatal) so one
// bad tenant does not block all others from loading.
func (m *TenantDBManager) LoadAll(ctx context.Context) error {
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants from global DB: %w", err)
	}
	if len(tenantDSNs) == 0 {
		m.logger.Warn("no tenants found in global DB tenants table")
		return nil
	}

	stores := make(map[string]configstore.ConfigStore, len(tenantDSNs))
	for tenantID, dsn := range tenantDSNs {
		store, storeErr := configstore.NewPostgresConfigStoreFromDSN(ctx, dsn, m.poolSettings, m.logger)
		if storeErr != nil {
			m.logger.Warn("skipping tenant %s: failed to open config store: %v", tenantID, storeErr)
			continue
		}
		stores[tenantID] = store
		m.logger.Info("tenant store loaded: %s", tenantID)
	}

	m.mu.Lock()
	m.stores = stores
	m.mu.Unlock()

	m.logger.Info("tenant stores loaded: %d/%d tenants ready", len(stores), len(tenantDSNs))
	return nil
}

// ListTenantIDs returns the tenant IDs currently registered in memory.
func (m *TenantDBManager) ListTenantIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.stores))
	for tenantID := range m.stores {
		ids = append(ids, tenantID)
	}
	return ids
}

// SyncTenantsFromGlobalDB opens ConfigStores for tenants discovered in the global
// tenants table that are not yet present in memory. Existing tenant connections
// are left untouched.
func (m *TenantDBManager) SyncTenantsFromGlobalDB(ctx context.Context) error {
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants from global DB: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	added := 0
	for tenantID, dsn := range tenantDSNs {
		if _, exists := m.stores[tenantID]; exists {
			continue
		}
		store, storeErr := configstore.NewPostgresConfigStoreFromDSN(ctx, dsn, m.poolSettings, m.logger)
		if storeErr != nil {
			m.logger.Warn("skipping tenant %s during sync: failed to open config store: %v", tenantID, storeErr)
			continue
		}
		m.stores[tenantID] = store
		added++
		m.logger.Info("tenant store loaded during sync: %s", tenantID)
	}
	if added > 0 {
		m.logger.Info("tenant sync added %d store(s); %d tenant(s) ready", added, len(m.stores))
	}
	return nil
}

// GetStore returns the pre-loaded ConfigStore for the given tenantID.
// Returns an error when the tenant was not found in the startup snapshot.
// Use EvictAndReload to refresh a single tenant at runtime.
func (m *TenantDBManager) GetStore(_ context.Context, tenantID string) (configstore.ConfigStore, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenantID must not be empty")
	}
	m.mu.RLock()
	store, exists := m.stores[tenantID]
	m.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("tenant %q not found (was it registered at startup?)", tenantID)
	}
	return store, nil
}

// EvictAndReload re-connects a single tenant's ConfigStore from the global DB.
// Useful when a tenant's database is migrated without restarting the gateway.
func (m *TenantDBManager) EvictAndReload(ctx context.Context, tenantID string) error {
	dsn, err := m.globalDB.GetTenantDSN(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("failed to resolve DSN for tenant %q: %w", tenantID, err)
	}
	store, err := configstore.NewPostgresConfigStoreFromDSN(ctx, dsn, m.poolSettings, m.logger)
	if err != nil {
		return fmt.Errorf("failed to open config store for tenant %q: %w", tenantID, err)
	}

	m.mu.Lock()
	old := m.stores[tenantID]
	m.stores[tenantID] = store
	m.mu.Unlock()

	if old != nil {
		if closeErr := old.Close(ctx); closeErr != nil {
			m.logger.Warn("error closing old store for tenant %s: %v", tenantID, closeErr)
		}
	}
	m.logger.Info("tenant store reloaded: %s", tenantID)
	return nil
}

// Close closes all tenant stores. Called during graceful shutdown.
func (m *TenantDBManager) Close(ctx context.Context) {
	m.mu.Lock()
	stores := m.stores
	m.stores = make(map[string]configstore.ConfigStore)
	m.mu.Unlock()

	for tenantID, store := range stores {
		if store != nil {
			if err := store.Close(ctx); err != nil {
				m.logger.Warn("error closing store for tenant %s during shutdown: %v", tenantID, err)
			}
		}
	}
}
