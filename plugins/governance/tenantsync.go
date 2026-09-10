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
	if p.ownsLocalBudgetUsage() {
		if err := localStore.DumpBudgets(ctx, nil); err != nil {
			p.logger.Warn("tenant governance sync: failed to dump budgets for tenant %s: %v", tenantID, err)
		}
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

func (p *GovernancePlugin) initTenantGovernanceComponents(ctx context.Context, tenantID string, configStore configstore.ConfigStore) *tenantGovernanceComponents {
	if raw, ok := p.tenantComponents.Load(tenantID); ok {
		if comp, ok := raw.(*tenantGovernanceComponents); ok && comp != nil {
			return comp
		}
	}

	store, err := NewLocalGovernanceStore(ctx, p.logger, configStore, nil, p.modelCatalog)
	if err != nil {
		p.logger.Debug("failed to initialise governance store for tenant %s: %v", tenantID, err)
		return nil
	}
	store.SetSyncBudgetUsageFromDatabase(p.isAIInfraDeployment())
	resolver := NewBudgetResolver(store, p.modelCatalog, p.logger, p.inMemoryStore)
	tracker := NewUsageTracker(p.ctx, store, resolver, configStore, p.logger)
	tracker.SetOwnsLocalBudgetUsage(p.ownsLocalBudgetUsage())
	engine, err := NewRoutingEngine(store, p.logger, p.routingChainMaxDepth)
	if err != nil {
		p.logger.Debug("failed to initialise routing engine for tenant %s: %v", tenantID, err)
		return nil
	}

	comp := &tenantGovernanceComponents{
		store:    store,
		resolver: resolver,
		tracker:  tracker,
		engine:   engine,
	}
	if actual, loaded := p.tenantComponents.LoadOrStore(tenantID, comp); loaded {
		if existing, ok := actual.(*tenantGovernanceComponents); ok && existing != nil {
			return existing
		}
	}

	p.logger.Info("tenant governance store initialised for tenant %s", tenantID)
	return comp
}
