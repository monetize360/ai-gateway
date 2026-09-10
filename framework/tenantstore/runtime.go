package tenantstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

const DefaultPoolIdleTimeout = 15 * time.Minute

// TenantStoreOpenedFunc is invoked after a tenant ConfigStore is opened for the
// first time (or reopened after idle eviction). It runs before GetStore returns
// so the first request can use freshly hydrated in-memory provider config.
type TenantStoreOpenedFunc func(ctx context.Context, tenantID string, store configstore.ConfigStore)

// TenantStoreClosingFunc is invoked just before a tenant ConfigStore is closed,
// while its pool is still usable, so listeners can flush in-memory state to the
// tenant DB and drop caches that reference the outgoing store.
type TenantStoreClosingFunc func(ctx context.Context, tenantID string, store configstore.ConfigStore)

type tenantStoreEntry struct {
	store    configstore.ConfigStore
	lastUsed atomic.Int64
}

func (e *tenantStoreEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

func (e *tenantStoreEntry) lastUsedAt() time.Time {
	return time.Unix(0, e.lastUsed.Load())
}

// TenantDBManager holds a map of configstore.ConfigStore keyed by tenantID.
// Stores are opened on first GetStore (single-flight) and closed after idle timeout
// or when the tenant is soft-deleted in the global tenants table.
type TenantDBManager struct {
	mu             sync.RWMutex
	stores         map[string]*tenantStoreEntry
	opening        map[string]chan struct{}
	globalDB       *GlobalDB
	poolSettings   configstore.PostgresPoolSettings
	logger         schemas.Logger
	onStoreOpened  TenantStoreOpenedFunc
	onStoreClosing TenantStoreClosingFunc
}

// NewTenantDBManager creates a TenantDBManager backed by the supplied GlobalDB.
func NewTenantDBManager(globalDB *GlobalDB, pool configstore.PostgresPoolSettings, logger schemas.Logger) *TenantDBManager {
	return &TenantDBManager{
		stores:       make(map[string]*tenantStoreEntry),
		opening:      make(map[string]chan struct{}),
		globalDB:     globalDB,
		poolSettings: pool,
		logger:       logger,
	}
}

// SetOnStoreOpened registers a callback invoked once after each successful open.
func (m *TenantDBManager) SetOnStoreOpened(fn TenantStoreOpenedFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onStoreOpened = fn
	m.mu.Unlock()
}

// SetOnStoreClosing registers a callback invoked before each tenant store is closed.
func (m *TenantDBManager) SetOnStoreClosing(fn TenantStoreClosingFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onStoreClosing = fn
	m.mu.Unlock()
}

// closeStore runs the closing hook against the still-open pool, then closes it.
func (m *TenantDBManager) closeStore(ctx context.Context, tenantID string, store configstore.ConfigStore, reason string) {
	if store == nil {
		return
	}
	m.mu.RLock()
	onClosing := m.onStoreClosing
	m.mu.RUnlock()
	if onClosing != nil {
		onClosing(ctx, tenantID, store)
	}
	if err := store.Close(ctx); err != nil {
		m.logger.Warn("error closing tenant store %s (%s): %v", tenantID, reason, err)
	}
}

// ListTenantIDs returns the tenant IDs that currently have an open pool.
func (m *TenantDBManager) ListTenantIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.stores))
	for tenantID := range m.stores {
		ids = append(ids, tenantID)
	}
	return ids
}

// SyncTenantsFromGlobalDB closes pools for tenants that have been soft-deleted
// (no longer returned by GetAllTenants). It does not open pools for new tenants;
// those are created on demand by GetStore.
func (m *TenantDBManager) SyncTenantsFromGlobalDB(ctx context.Context) error {
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants from global DB: %w", err)
	}

	m.mu.RLock()
	var toEvict []struct {
		id    string
		store configstore.ConfigStore
	}
	for tenantID, entry := range m.stores {
		if _, active := tenantDSNs[tenantID]; !active {
			toEvict = append(toEvict, struct {
				id    string
				store configstore.ConfigStore
			}{tenantID, entry.store})
		}
	}
	m.mu.RUnlock()

	for _, entry := range toEvict {
		m.removeStore(entry.id)
		m.closeStore(ctx, entry.id, entry.store, "soft-deleted")
		m.logger.Info("tenant store evicted (soft-deleted): %s", entry.id)
	}
	return nil
}

// GetStore returns the ConfigStore for tenantID, opening a pool on first access.
func (m *TenantDBManager) GetStore(ctx context.Context, tenantID string) (configstore.ConfigStore, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenantID must not be empty")
	}

	if store := m.storeIfOpen(tenantID); store != nil {
		return store, nil
	}

	for {
		m.mu.Lock()
		if entry, exists := m.stores[tenantID]; exists {
			entry.touch()
			store := entry.store
			m.mu.Unlock()
			return store, nil
		}
		if wait, opening := m.opening[tenantID]; opening {
			m.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if store := m.storeIfOpen(tenantID); store != nil {
				return store, nil
			}
			continue
		}
		wait := make(chan struct{})
		m.opening[tenantID] = wait
		m.mu.Unlock()

		store, err := m.openStore(ctx, tenantID)
		m.mu.Lock()
		delete(m.opening, tenantID)
		onOpened := m.onStoreOpened
		if err == nil && store != nil {
			entry := &tenantStoreEntry{store: store}
			entry.touch()
			m.stores[tenantID] = entry
		}
		m.mu.Unlock()

		func() {
			defer close(wait)
			if err == nil && onOpened != nil {
				onOpened(ctx, tenantID, store)
			}
		}()
		if err != nil {
			return nil, err
		}
		m.logger.Info("tenant store opened on demand: %s", tenantID)
		return store, nil
	}
}

// PeekStore returns the open ConfigStore for tenantID without opening a pool
// or updating lastUsed. Background workers must use this so idle eviction can
// still fire.
func (m *TenantDBManager) PeekStore(tenantID string) configstore.ConfigStore {
	if m == nil || tenantID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry := m.stores[tenantID]
	if entry == nil {
		return nil
	}
	return entry.store
}

func (m *TenantDBManager) storeIfOpen(tenantID string) configstore.ConfigStore {
	m.mu.RLock()
	entry, exists := m.stores[tenantID]
	m.mu.RUnlock()
	if !exists || entry == nil {
		return nil
	}
	entry.touch()
	return entry.store
}

func (m *TenantDBManager) openStore(ctx context.Context, tenantID string) (configstore.ConfigStore, error) {
	dsn, err := m.globalDB.GetTenantDSN(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	store, err := configstore.NewPostgresConfigStoreFromDSN(ctx, dsn, m.poolSettings, m.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to open config store for tenant %q: %w", tenantID, err)
	}
	return store, nil
}

// EvictIdle closes tenant pools that have not been used for idleTimeout.
// Returns the tenant IDs whose pools were closed.
func (m *TenantDBManager) EvictIdle(ctx context.Context, idleTimeout time.Duration) []string {
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

		m.closeStore(ctx, tenantID, store, "idle")
		m.logger.Info("tenant store evicted (idle): %s", tenantID)
		evicted = append(evicted, tenantID)
	}
	return evicted
}

func (m *TenantDBManager) removeStore(tenantID string) {
	m.mu.Lock()
	delete(m.stores, tenantID)
	m.mu.Unlock()
}

// EvictAndReload re-connects a single tenant's ConfigStore from the global DB.
func (m *TenantDBManager) EvictAndReload(ctx context.Context, tenantID string) error {
	store, err := m.openStore(ctx, tenantID)
	if err != nil {
		return err
	}

	m.mu.Lock()
	old := m.stores[tenantID]
	entry := &tenantStoreEntry{store: store}
	entry.touch()
	m.stores[tenantID] = entry
	onOpened := m.onStoreOpened
	m.mu.Unlock()

	if old != nil {
		m.closeStore(ctx, tenantID, old.store, "reloaded")
	}
	if onOpened != nil {
		onOpened(ctx, tenantID, store)
	}
	m.logger.Info("tenant store reloaded: %s", tenantID)
	return nil
}

// Close closes all tenant stores. Called during graceful shutdown.
func (m *TenantDBManager) Close(ctx context.Context) {
	m.mu.Lock()
	stores := m.stores
	m.stores = make(map[string]*tenantStoreEntry)
	m.mu.Unlock()

	for tenantID, entry := range stores {
		if entry != nil && entry.store != nil {
			if err := entry.store.Close(ctx); err != nil {
				m.logger.Warn("error closing store for tenant %s during shutdown: %v", tenantID, err)
			}
		}
	}
}
