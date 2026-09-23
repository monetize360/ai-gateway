package tenantstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/asyncjob"
)

type tenantAsyncJobEntry struct {
	store    asyncjob.Store
	lastUsed atomic.Int64
}

func (e *tenantAsyncJobEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

func (e *tenantAsyncJobEntry) lastUsedAt() time.Time {
	return time.Unix(0, e.lastUsed.Load())
}

// TenantAsyncJobManager holds one postgres async job store per tenant DB.
type TenantAsyncJobManager struct {
	mu           sync.RWMutex
	stores       map[string]*tenantAsyncJobEntry
	opening      map[string]chan struct{}
	globalDB     *GlobalDB
	poolSettings asyncjob.PoolSettings
	logger       schemas.Logger
}

func NewTenantAsyncJobManager(globalDB *GlobalDB, pool asyncjob.PoolSettings, logger schemas.Logger) *TenantAsyncJobManager {
	return &TenantAsyncJobManager{
		stores:       make(map[string]*tenantAsyncJobEntry),
		opening:      make(map[string]chan struct{}),
		globalDB:     globalDB,
		poolSettings: pool,
		logger:       logger,
	}
}

func (m *TenantAsyncJobManager) SyncTenantsFromGlobalDB(ctx context.Context) error {
	if m == nil || m.globalDB == nil {
		return nil
	}
	tenantDSNs, err := m.globalDB.GetAllTenants(ctx)
	if err != nil {
		return fmt.Errorf("failed to load tenants for async job store sync: %w", err)
	}

	m.mu.RLock()
	var toEvict []struct {
		id    string
		store asyncjob.Store
	}
	for tenantID, entry := range m.stores {
		if _, active := tenantDSNs[tenantID]; !active {
			toEvict = append(toEvict, struct {
				id    string
				store asyncjob.Store
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
				m.logger.Warn("error closing evicted tenant async job store %s: %v", entry.id, closeErr)
			}
		}
		m.logger.Info("tenant async job store evicted (soft-deleted): %s", entry.id)
	}
	return nil
}

func (m *TenantAsyncJobManager) getOrOpen(ctx context.Context, tenantID string) asyncjob.Store {
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
			entry := &tenantAsyncJobEntry{store: store}
			entry.touch()
			m.stores[tenantID] = entry
			m.logger.Info("tenant async job store opened on demand: %s", tenantID)
		} else if err != nil {
			m.logger.Warn("failed to open tenant async job store %s: %v", tenantID, err)
		}
		m.mu.Unlock()
		close(wait)
		return store
	}
}

func (m *TenantAsyncJobManager) storeIfOpen(tenantID string) asyncjob.Store {
	m.mu.RLock()
	entry, exists := m.stores[tenantID]
	m.mu.RUnlock()
	if !exists || entry == nil {
		return nil
	}
	entry.touch()
	return entry.store
}

func (m *TenantAsyncJobManager) openStore(ctx context.Context, tenantID string) (asyncjob.Store, error) {
	dsn, err := m.globalDB.GetTenantDSN(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return asyncjob.NewPostgresStoreFromDSN(ctx, dsn, m.poolSettings, m.logger)
}

func (m *TenantAsyncJobManager) GetStoreFromContext(ctx context.Context) asyncjob.Store {
	return m.getOrOpen(ctx, TenantIDFromContext(ctx))
}

func (m *TenantAsyncJobManager) ForEachStore(fn func(tenantID string, store asyncjob.Store)) {
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

func (m *TenantAsyncJobManager) EvictIdle(ctx context.Context, idleTimeout time.Duration) []string {
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
				m.logger.Warn("error closing idle tenant async job store %s: %v", tenantID, closeErr)
			}
		}
		m.logger.Info("tenant async job store evicted (idle): %s", tenantID)
		evicted = append(evicted, tenantID)
	}
	return evicted
}

func (m *TenantAsyncJobManager) Close(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	stores := m.stores
	m.stores = make(map[string]*tenantAsyncJobEntry)
	m.mu.Unlock()
	for tenantID, entry := range stores {
		if entry != nil && entry.store != nil {
			if err := entry.store.Close(ctx); err != nil {
				m.logger.Warn("error closing async job store for tenant %s: %v", tenantID, err)
			}
		}
	}
}
