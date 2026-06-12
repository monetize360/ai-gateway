package tenantstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/logstore"
)

// LogStorePoolSettings tunes per-tenant log store connection pools.
type LogStorePoolSettings struct {
	MaxIdleConns int
	MaxOpenConns int
}

// LogStoreResolver resolves finops log stores from tenant context.
type LogStoreResolver interface {
	GetLogStoreFromContext(ctx context.Context) logstore.LogStore
	GetLogStoreForTenant(tenantID string) logstore.LogStore
	ForEachStore(fn func(tenantID string, store logstore.LogStore))
	SyncTenantsFromGlobalDB(ctx context.Context) error
	ListTenantIDs() []string
	Ping(ctx context.Context) error
	Close(ctx context.Context)
}

// TenantLogStoreManager holds one postgres LogStore per tenant DB.
type TenantLogStoreManager struct {
	mu           sync.RWMutex
	stores       map[string]logstore.LogStore
	globalDB     *GlobalDB
	poolSettings LogStorePoolSettings
	logger       schemas.Logger
}

// NewTenantLogStoreManager creates a manager backed by the global tenants table.
func NewTenantLogStoreManager(globalDB *GlobalDB, pool LogStorePoolSettings, logger schemas.Logger) *TenantLogStoreManager {
	return &TenantLogStoreManager{
		stores:       make(map[string]logstore.LogStore),
		globalDB:     globalDB,
		poolSettings: pool,
		logger:       logger,
	}
}

// LoadAll opens a LogStore for every tenant discovered in the global DB.
func (m *TenantLogStoreManager) LoadAll(ctx context.Context) error {
	if m == nil || m.globalDB == nil {
		return fmt.Errorf("tenant log store manager is not configured")
	}
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants for log stores: %w", err)
	}
	stores := make(map[string]logstore.LogStore, len(tenantDSNs))
	for tenantID, dsn := range tenantDSNs {
		store, storeErr := logstore.NewPostgresLogStoreFromDSN(ctx, dsn, m.poolSettings.MaxIdleConns, m.poolSettings.MaxOpenConns, m.logger)
		if storeErr != nil {
			m.logger.Warn("skipping tenant %s log store: %v", tenantID, storeErr)
			continue
		}
		stores[tenantID] = store
		m.logger.Info("tenant log store loaded: %s", tenantID)
	}
	m.mu.Lock()
	m.stores = stores
	m.mu.Unlock()
	m.logger.Info("tenant log stores loaded: %d/%d tenants ready", len(stores), len(tenantDSNs))
	return nil
}

// SyncTenantsFromGlobalDB opens log stores for newly registered tenants.
func (m *TenantLogStoreManager) SyncTenantsFromGlobalDB(ctx context.Context) error {
	if m == nil || m.globalDB == nil {
		return nil
	}
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants for log store sync: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0
	for tenantID, dsn := range tenantDSNs {
		if _, exists := m.stores[tenantID]; exists {
			continue
		}
		store, storeErr := logstore.NewPostgresLogStoreFromDSN(ctx, dsn, m.poolSettings.MaxIdleConns, m.poolSettings.MaxOpenConns, m.logger)
		if storeErr != nil {
			m.logger.Warn("skipping tenant %s during log store sync: %v", tenantID, storeErr)
			continue
		}
		m.stores[tenantID] = store
		added++
		m.logger.Info("tenant log store loaded during sync: %s", tenantID)
	}
	if added > 0 {
		m.logger.Info("tenant log store sync added %d store(s); %d tenant(s) ready", added, len(m.stores))
	}
	return nil
}

// GetLogStoreForTenant returns the LogStore for tenantID.
func (m *TenantLogStoreManager) GetLogStoreForTenant(tenantID string) logstore.LogStore {
	if m == nil || tenantID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stores[tenantID]
}

// GetLogStoreFromContext resolves the LogStore for the tenant on ctx.
func (m *TenantLogStoreManager) GetLogStoreFromContext(ctx context.Context) logstore.LogStore {
	return m.GetLogStoreForTenant(TenantIDFromContext(ctx))
}

// ForEachStore invokes fn for every loaded tenant log store.
func (m *TenantLogStoreManager) ForEachStore(fn func(tenantID string, store logstore.LogStore)) {
	if m == nil || fn == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for tenantID, store := range m.stores {
		if store != nil {
			fn(tenantID, store)
		}
	}
}

// ListTenantIDs returns tenant IDs with loaded log stores.
func (m *TenantLogStoreManager) ListTenantIDs() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.stores))
	for tenantID := range m.stores {
		ids = append(ids, tenantID)
	}
	return ids
}

// Ping checks connectivity for every tenant log store.
func (m *TenantLogStoreManager) Ping(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("tenant log store manager is not configured")
	}
	m.mu.RLock()
	stores := make([]logstore.LogStore, 0, len(m.stores))
	for _, store := range m.stores {
		if store != nil {
			stores = append(stores, store)
		}
	}
	m.mu.RUnlock()
	if len(stores) == 0 {
		return fmt.Errorf("no tenant log stores loaded")
	}
	var firstErr error
	for _, store := range stores {
		if err := store.Ping(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close closes all tenant log stores.
func (m *TenantLogStoreManager) Close(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	stores := m.stores
	m.stores = make(map[string]logstore.LogStore)
	m.mu.Unlock()
	for tenantID, store := range stores {
		if store != nil {
			if err := store.Close(ctx); err != nil {
				m.logger.Warn("error closing log store for tenant %s: %v", tenantID, err)
			}
		}
	}
}
