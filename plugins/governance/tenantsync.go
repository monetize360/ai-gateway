package governance

import (
	"context"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
)

// TenantGovernanceSyncSource extends TenantConfigProvider with tenant enumeration
// and registry refresh used by the periodic governance sync worker.
type TenantGovernanceSyncSource interface {
	TenantConfigProvider
	ListTenantIDs(ctx context.Context) []string
	SyncTenants(ctx context.Context) error
	GetStoreForTenant(ctx context.Context, tenantID string) configstore.ConfigStore
}

// StartTenantGovernanceSync launches a background worker that reloads governance
// data from each tenant's config store on the given interval. It is a no-op when
// provider is nil or interval <= 0.
func (p *GovernancePlugin) StartTenantGovernanceSync(interval time.Duration) {
	if p == nil || interval <= 0 {
		return
	}
	source, ok := p.tenantConfigProvider.(TenantGovernanceSyncSource)
	if !ok || source == nil {
		p.logger.Warn("tenant governance sync skipped: provider does not implement TenantGovernanceSyncSource")
		return
	}

	p.tenantSyncOnce.Do(func() {
		ctx, cancel := context.WithCancel(p.ctx)
		p.tenantSyncCancel = cancel
		p.wg.Add(1)
		go p.tenantGovernanceSyncWorker(ctx, interval, source)
		p.logger.Info("tenant governance sync started (interval=%s)", interval)
	})
}

func (p *GovernancePlugin) tenantGovernanceSyncWorker(ctx context.Context, interval time.Duration, source TenantGovernanceSyncSource) {
	defer p.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.syncAllTenantGovernanceStores(ctx, source)

	for {
		select {
		case <-ticker.C:
			p.syncAllTenantGovernanceStores(ctx, source)
		case <-ctx.Done():
			return
		}
	}
}

func (p *GovernancePlugin) syncAllTenantGovernanceStores(ctx context.Context, source TenantGovernanceSyncSource) {
	if err := source.SyncTenants(ctx); err != nil {
		p.logger.Warn("tenant governance sync: failed to refresh tenant registry: %v", err)
	}

	tenantIDs := source.ListTenantIDs(ctx)
	for _, tenantID := range tenantIDs {
		configStore := source.GetStoreForTenant(ctx, tenantID)
		if configStore == nil {
			p.logger.Warn("tenant governance sync: no config store for tenant %s", tenantID)
			continue
		}
		if err := p.syncTenantGovernanceStore(ctx, tenantID, configStore); err != nil {
			p.logger.Warn("tenant governance sync failed for tenant %s: %v", tenantID, err)
		}
	}
}

func (p *GovernancePlugin) syncTenantGovernanceStore(ctx context.Context, tenantID string, configStore configstore.ConfigStore) error {
	store := p.initTenantGovernanceStore(ctx, tenantID, configStore)
	if store == nil {
		return nil
	}

	localStore, ok := store.(*LocalGovernanceStore)
	if !ok {
		return nil
	}

	if err := localStore.DumpRateLimits(ctx, nil, nil); err != nil {
		p.logger.Warn("tenant governance sync: failed to dump rate limits for tenant %s: %v", tenantID, err)
	}
	if err := localStore.DumpBudgets(ctx, nil); err != nil {
		p.logger.Warn("tenant governance sync: failed to dump budgets for tenant %s: %v", tenantID, err)
	}
	return localStore.RefreshFromDatabase(ctx)
}

func (p *GovernancePlugin) initTenantGovernanceStore(ctx context.Context, tenantID string, configStore configstore.ConfigStore) GovernanceStore {
	if raw, ok := p.tenantComponents.Load(tenantID); ok {
		if comp, ok := raw.(*tenantGovernanceComponents); ok && comp != nil {
			return comp.store
		}
	}

	store, err := NewLocalGovernanceStore(ctx, p.logger, configStore, nil, p.modelCatalog)
	if err != nil {
		p.logger.Warn("failed to initialise governance store for tenant %s: %v", tenantID, err)
		return nil
	}
	resolver := NewBudgetResolver(store, p.modelCatalog, p.logger, p.inMemoryStore)

	comp := &tenantGovernanceComponents{store: store, resolver: resolver}
	if actual, loaded := p.tenantComponents.LoadOrStore(tenantID, comp); loaded {
		if existing, ok := actual.(*tenantGovernanceComponents); ok && existing != nil {
			return existing.store
		}
	}

	p.logger.Info("tenant governance store initialised for tenant %s", tenantID)
	return store
}
