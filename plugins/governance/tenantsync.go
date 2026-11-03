package governance

import (
	"context"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
)

// StartTenantGovernanceSync launches a background worker that reloads governance
// data from each tenant's config store on the given interval.
func (p *GovernancePlugin) StartTenantGovernanceSync(interval time.Duration) {
	if p == nil || interval <= 0 || p.registry == nil {
		return
	}

	p.tenantSyncOnce.Do(func() {
		ctx, cancel := context.WithCancel(p.ctx)
		p.tenantSyncCancel = cancel
		p.wg.Add(1)
		go p.tenantGovernanceSyncWorker(ctx, interval)
		p.logger.Info("tenant governance sync started (interval=%s)", interval)
	})
}

func (p *GovernancePlugin) tenantGovernanceSyncWorker(ctx context.Context, interval time.Duration) {
	defer p.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.syncAllTenantGovernanceStores(ctx)

	for {
		select {
		case <-ticker.C:
			p.syncAllTenantGovernanceStores(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *GovernancePlugin) syncAllTenantGovernanceStores(ctx context.Context) {
	if p.registry == nil {
		return
	}
	if err := p.registry.SyncTenants(ctx); err != nil {
		p.logger.Warn("tenant governance sync: failed to refresh tenant registry: %v", err)
	}

	tenantIDs := p.registry.ListTenantIDs(ctx)
	for _, tenantID := range tenantIDs {
		configStore := p.registry.PeekStoreForTenant(tenantID)
		if configStore == nil {
			continue
		}
		if err := p.syncTenantGovernanceStore(ctx, tenantID, configStore); err != nil {
			p.logger.Debug("tenant governance sync failed for tenant %s: %v", tenantID, err)
			continue
		}

	}
}

func (p *GovernancePlugin) syncTenantGovernanceStore(ctx context.Context, tenantID string, configStore configstore.ConfigStore) error {
	comp := p.initTenantGovernanceComponents(ctx, tenantID, configStore)
	if comp == nil || comp.store == nil {
		return nil
	}

	localStore, ok := comp.store.(*LocalGovernanceStore)
	if !ok {
		return nil
	}

	if err := localStore.DumpRateLimits(ctx, nil, nil); err != nil {
		p.logger.Warn("tenant governance sync: failed to dump rate limits for tenant %s: %v", tenantID, err)
	}
	return localStore.RefreshFromDatabase(ctx)
}

// ReleaseTenant drops the cached governance components for tenantID. It must be
// called while the tenant's config store is still open: the usage tracker's
// cleanup flushes pending budget and rate-limit deltas back to the tenant DB.
// The next request for this tenant rebuilds the components against the new pool.
func (p *GovernancePlugin) ReleaseTenant(tenantID string) {
	if p == nil || tenantID == "" {
		return
	}
	raw, loaded := p.tenantComponents.LoadAndDelete(tenantID)
	if !loaded {
		return
	}
	comp, ok := raw.(*tenantGovernanceComponents)
	if !ok || comp == nil || comp.tracker == nil {
		return
	}
	if err := comp.tracker.Cleanup(); err != nil {
		p.logger.Warn("tenant governance release: usage tracker cleanup failed for tenant %s: %v", tenantID, err)
	}
	p.logger.Info("tenant governance components released for tenant %s", tenantID)
}

// tenantInitTimeout bounds one tenant's governance bootstrap. The bootstrap is
// shared by every request waiting on it, so it must not inherit one request's
// cancellation.
const tenantInitTimeout = 30 * time.Second

type tenantInitCall struct {
	done chan struct{}
	comp *tenantGovernanceComponents
}

// initTenantGovernanceComponents returns the tenant's components, building them
// at most once at a time per tenant. Concurrent callers wait for the in-flight
// build instead of each loading the tenant from Postgres and starting a usage
// tracker of their own.
func (p *GovernancePlugin) initTenantGovernanceComponents(ctx context.Context, tenantID string, configStore configstore.ConfigStore) *tenantGovernanceComponents {
	if comp := p.loadTenantComponents(tenantID); comp != nil {
		return comp
	}

	call := &tenantInitCall{done: make(chan struct{})}
	if actual, loaded := p.tenantInits.LoadOrStore(tenantID, call); loaded {
		inFlight := actual.(*tenantInitCall)
		select {
		case <-inFlight.done:
			return inFlight.comp
		case <-ctx.Done():
			return nil
		}
	}
	defer func() {
		close(call.done)
		p.tenantInits.CompareAndDelete(tenantID, call)
	}()

	// A build may have finished between the first check and winning the slot.
	if comp := p.loadTenantComponents(tenantID); comp != nil {
		call.comp = comp
		return comp
	}
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tenantInitTimeout)
	defer cancel()
	// The pool can be closed or replaced while the build runs. ReleaseTenant
	// cannot drop components that are not stored yet, so the build checks for
	// this itself and asks to be retried against the tenant's current pool.
	const maxBuildAttempts = 2
	for attempt := 0; attempt < maxBuildAttempts && configStore != nil; attempt++ {
		comp, stale := p.buildTenantGovernanceComponents(buildCtx, tenantID, configStore)
		if !stale {
			call.comp = comp
			return comp
		}
		p.logger.Warn("tenant store for tenant %s was closed during governance bootstrap; retrying against the current pool", tenantID)
		configStore = p.registry.GetStoreForTenant(buildCtx, tenantID)
	}
	return nil
}

// tenantStoreIsCurrent reports whether configStore is still the tenant's open
// pool. The pool manager removes a pool from its map before running the
// closing hook (ReleaseTenant), so a build that stored its components and then
// sees its pool here is guaranteed that any later close will release them.
func (p *GovernancePlugin) tenantStoreIsCurrent(tenantID string, configStore configstore.ConfigStore) bool {
	if p.registry == nil {
		return true
	}
	return p.registry.PeekStoreForTenant(tenantID) == configStore
}

func (p *GovernancePlugin) loadTenantComponents(tenantID string) *tenantGovernanceComponents {
	if raw, ok := p.tenantComponents.Load(tenantID); ok {
		if comp, ok := raw.(*tenantGovernanceComponents); ok && comp != nil {
			return comp
		}
	}
	return nil
}

// buildTenantGovernanceComponents builds and stores the tenant's components.
// stale is true when configStore stopped being the tenant's open pool during
// the build; nothing is kept in that case and the caller should retry against
// the current pool.
func (p *GovernancePlugin) buildTenantGovernanceComponents(ctx context.Context, tenantID string, configStore configstore.ConfigStore) (built *tenantGovernanceComponents, stale bool) {
	var store *LocalGovernanceStore
	var err error
	if p.redisCache != nil {
		snapshot, cacheErr := p.redisCache.load(ctx, tenantID)
		if cacheErr != nil {
			p.logger.Warn("failed to load governance cache for tenant %s: %v", tenantID, cacheErr)
		} else if snapshot != nil {
			store, err = newEmptyLocalGovernanceStore(p.logger, configStore, p.modelCatalog)
			if err == nil {
				supplemental, supErr := store.loadSupplementalFromDatabase(ctx)
				if supErr != nil {
					// Without these rows access control and alerts would be wrong;
					// fall back to the full database load below.
					p.logger.Warn("failed to load database-only governance rows for tenant %s: %v", tenantID, supErr)
					store = nil
				} else {
					store.ReplaceFromRedisSnapshot(ctx, snapshot, supplemental)
					p.logger.Info("tenant governance store loaded from Redis for tenant %s (version=%d)", tenantID, snapshot.Version)
				}
			}
		}
	}
	if store == nil && err == nil {
		store, err = NewLocalGovernanceStore(ctx, p.logger, configStore, nil, p.modelCatalog)
	}
	if err != nil {
		p.logger.Debug("failed to initialise governance store for tenant %s: %v", tenantID, err)
		return nil, false
	}
	resolver := NewBudgetResolver(store, p.modelCatalog, p.logger, p.inMemoryStore)
	tracker := NewUsageTracker(p.ctx, store, resolver, configStore, p.logger)
	tracker.SetTenantID(tenantID)
	tracker.SetAlertPublisher(p.usagePublisher)
	engine, err := NewRoutingEngine(store, p.logger, p.routingChainMaxDepth)
	if err != nil {
		p.logger.Debug("failed to initialise routing engine for tenant %s: %v", tenantID, err)
		tracker.Stop()
		return nil, false
	}

	comp := &tenantGovernanceComponents{
		store:    store,
		resolver: resolver,
		tracker:  tracker,
		engine:   engine,
	}
	if actual, loaded := p.tenantComponents.LoadOrStore(tenantID, comp); loaded {
		// Lost a race (e.g. against ReleaseTenant + rebuild). Stop this tracker's
		// workers without its final dump: its counters were never used and would
		// overwrite the winner's newer values in the database.
		tracker.Stop()
		if existing, ok := actual.(*tenantGovernanceComponents); ok && existing != nil {
			return existing, false
		}
		return nil, false
	}

	// Checked after storing: if the pool is still current now, a later close
	// runs ReleaseTenant, which finds and releases these components. If it is
	// not, ReleaseTenant may already have run and missed them, so drop them
	// here rather than serve a tenant from a closed pool indefinitely.
	if !p.tenantStoreIsCurrent(tenantID, configStore) {
		if p.tenantComponents.CompareAndDelete(tenantID, comp) {
			// The pool is closed, so a final dump could not be written anyway.
			tracker.Stop()
		}
		return nil, true
	}

	p.logger.Info("tenant governance store initialised for tenant %s", tenantID)
	return comp, false
}
