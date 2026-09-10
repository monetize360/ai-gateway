package tenantstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/logstore"
)

// LogStorePoolSettings tunes per-tenant log store connection pools.
type LogStorePoolSettings struct {
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}

// LogStoreResolver resolves finops log stores from tenant context.
type LogStoreResolver interface {
	GetLogStoreFromContext(ctx context.Context) logstore.LogStore
	GetLogStoreForTenant(tenantID string) logstore.LogStore
	ForEachStore(fn func(tenantID string, store logstore.LogStore))
	SyncTenantsFromGlobalDB(ctx context.Context) error
	ListTenantIDs() []string
	EvictIdle(ctx context.Context, idleTimeout time.Duration) []string
	Ping(ctx context.Context) error
	Close(ctx context.Context)
}

type tenantLogStoreEntry struct {
	store    logstore.LogStore
	lastUsed atomic.Int64
}

func (e *tenantLogStoreEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

func (e *tenantLogStoreEntry) lastUsedAt() time.Time {
	return time.Unix(0, e.lastUsed.Load())
}

// TenantLogStoreManager holds one postgres LogStore per tenant DB, opened on demand.
type TenantLogStoreManager struct {
	mu           sync.RWMutex
	stores       map[string]*tenantLogStoreEntry
	opening      map[string]chan struct{}
	globalDB     *GlobalDB
	poolSettings LogStorePoolSettings
	logger       schemas.Logger
}

// NewTenantLogStoreManager creates a manager backed by the global tenants table.
func NewTenantLogStoreManager(globalDB *GlobalDB, pool LogStorePoolSettings, logger schemas.Logger) *TenantLogStoreManager {
	return &TenantLogStoreManager{
		stores:       make(map[string]*tenantLogStoreEntry),
		opening:      make(map[string]chan struct{}),
		globalDB:     globalDB,
		poolSettings: pool,
		logger:       logger,
	}
}

func (m *TenantLogStoreManager) dsnPool() logstore.PoolSettings {
	return logstore.PoolSettings{
		MaxIdleConns:    m.poolSettings.MaxIdleConns,
		MaxOpenConns:    m.poolSettings.MaxOpenConns,
		ConnMaxIdleTime: m.poolSettings.ConnMaxIdleTime,
		ConnMaxLifetime: m.poolSettings.ConnMaxLifetime,
	}
}

// SyncTenantsFromGlobalDB closes log stores for tenants that have been soft-deleted.
// It does not open stores for new tenants; those are created on demand.
func (m *TenantLogStoreManager) SyncTenantsFromGlobalDB(ctx context.Context) error {
	if m == nil || m.globalDB == nil {
		return nil
	}
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants for log store sync: %w", err)
	}

	m.mu.RLock()
	var toEvict []struct {
		id    string
		store logstore.LogStore
	}
	for tenantID, entry := range m.stores {
		if _, active := tenantDSNs[tenantID]; !active {
			toEvict = append(toEvict, struct {
				id    string
				store logstore.LogStore
			}{tenantID, entry.store})
		}
	}
	m.mu.RUnlock()

	for _, entry := range toEvict {
		m.mu.Lock()
		delete(m.stores, entry.id)
		m.mu.Unlock()
		if entry.store != nil {
			if closeErr := entry.store.Close(ctx); closeErr != nil {
				m.logger.Warn("error closing evicted tenant log store %s: %v", entry.id, closeErr)
			}
		}
		m.logger.Info("tenant log store evicted (soft-deleted): %s", entry.id)
	}
	return nil
}

func (m *TenantLogStoreManager) getOrOpen(ctx context.Context, tenantID string) logstore.LogStore {
	if m == nil || tenantID == "" {
		return nil
	}
	if store := m.storeIfOpen(tenantID); store != nil {
		return store
	}

	for {
		m.mu.Lock()
		if entry, exists := m.stores[tenantID]; exists {
			entry.touch()
			store := entry.store
			m.mu.Unlock()
			return store
		}
		if wait, opening := m.opening[tenantID]; opening {
			m.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil
			}
			if store := m.storeIfOpen(tenantID); store != nil {
				return store
			}
			continue
		}
		wait := make(chan struct{})
		m.opening[tenantID] = wait
		m.mu.Unlock()

		store, err := m.openStore(ctx, tenantID)
		m.mu.Lock()
		delete(m.opening, tenantID)
		if err == nil && store != nil {
			entry := &tenantLogStoreEntry{store: store}
			entry.touch()
			m.stores[tenantID] = entry
			m.logger.Info("tenant log store opened on demand: %s", tenantID)
		} else if err != nil {
			m.logger.Warn("failed to open tenant log store %s: %v", tenantID, err)
		}
		m.mu.Unlock()
		close(wait)
		return store
	}
}

func (m *TenantLogStoreManager) storeIfOpen(tenantID string) logstore.LogStore {
	m.mu.RLock()
	entry, exists := m.stores[tenantID]
	m.mu.RUnlock()
	if !exists || entry == nil {
		return nil
	}
	entry.touch()
	return entry.store
}

func (m *TenantLogStoreManager) openStore(ctx context.Context, tenantID string) (logstore.LogStore, error) {
	dsn, err := m.globalDB.GetTenantDSN(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return logstore.NewPostgresLogStoreFromDSN(ctx, dsn, m.dsnPool(), m.logger)
}

// GetLogStoreForTenant returns the LogStore for tenantID, opening a pool on first use.
func (m *TenantLogStoreManager) GetLogStoreForTenant(tenantID string) logstore.LogStore {
	return m.getOrOpen(context.Background(), tenantID)
}

// GetLogStoreFromContext resolves the LogStore for the tenant on ctx.
func (m *TenantLogStoreManager) GetLogStoreFromContext(ctx context.Context) logstore.LogStore {
	return m.getOrOpen(ctx, TenantIDFromContext(ctx))
}

// ForEachStore invokes fn for every loaded tenant log store.
func (m *TenantLogStoreManager) ForEachStore(fn func(tenantID string, store logstore.LogStore)) {
	if m == nil || fn == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for tenantID, entry := range m.stores {
		if entry != nil && entry.store != nil {
			fn(tenantID, entry.store)
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

// EvictIdle closes log stores that have not been used for idleTimeout.
func (m *TenantLogStoreManager) EvictIdle(ctx context.Context, idleTimeout time.Duration) []string {
	if m == nil || idleTimeout <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-idleTimeout)

	m.mu.RLock()
	candidates := make([]string, 0)
	for tenantID, entry := range m.stores {
		if entry.lastUsedAt().Before(cutoff) {
			candidates = append(candidates, tenantID)
		}
	}
	m.mu.RUnlock()

	evicted := make([]string, 0)
	for _, tenantID := range candidates {
		m.mu.Lock()
		entry, exists := m.stores[tenantID]
		if !exists || !entry.lastUsedAt().Before(cutoff) {
			m.mu.Unlock()
			continue
		}
		store := entry.store
		delete(m.stores, tenantID)
		m.mu.Unlock()

		if store != nil {
			if closeErr := store.Close(ctx); closeErr != nil {
				m.logger.Warn("error closing idle tenant log store %s: %v", tenantID, closeErr)
			}
		}
		m.logger.Info("tenant log store evicted (idle): %s", tenantID)
		evicted = append(evicted, tenantID)
	}
	return evicted
}

// Ping checks connectivity for every currently open tenant log store.
// An empty map is success: pools are opened on demand and none may exist yet.
func (m *TenantLogStoreManager) Ping(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("tenant log store manager is not configured")
	}
	m.mu.RLock()
	stores := make([]logstore.LogStore, 0, len(m.stores))
	for _, entry := range m.stores {
		if entry != nil && entry.store != nil {
			stores = append(stores, entry.store)
		}
	}
	m.mu.RUnlock()
	if len(stores) == 0 {
		return nil
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
	m.stores = make(map[string]*tenantLogStoreEntry)
	m.mu.Unlock()
	for tenantID, entry := range stores {
		if entry != nil && entry.store != nil {
			if err := entry.store.Close(ctx); err != nil {
				m.logger.Warn("error closing log store for tenant %s: %v", tenantID, err)
			}
		}
	}
}
