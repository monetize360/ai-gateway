package lib

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

// tenantProviderSync controls the background worker that hydrates in-memory
// provider configs from each tenant's config store.
type tenantProviderSync struct {
	once   sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var globalTenantProviderSync tenantProviderSync

// tenantProviderRuntimeUpdater updates the Bifrost client when tenant provider configs change.
type tenantProviderRuntimeUpdater interface {
	UpdateProvider(provider schemas.ModelProvider) error
}

// SyncAllTenantProvidersOnce loads provider configs from every registered tenant
// database into the in-memory store and registers them with the Bifrost client.
func SyncAllTenantProvidersOnce(ctx context.Context, cfg *Config, client tenantProviderRuntimeUpdater) {
	if cfg == nil || cfg.TenantStore == nil || cfg.TenantStore.Registry == nil {
		return
	}
	syncAllTenantProviders(ctx, cfg, client)
}

// StartTenantProviderSync launches a background worker that reloads provider
// configs from each tenant's config store on the given interval.
func StartTenantProviderSync(ctx context.Context, cfg *Config, client tenantProviderRuntimeUpdater, interval time.Duration) {
	if cfg == nil || client == nil || interval <= 0 || cfg.TenantStore == nil || cfg.TenantStore.Registry == nil {
		return
	}

	globalTenantProviderSync.once.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		globalTenantProviderSync.cancel = cancel
		globalTenantProviderSync.wg.Add(1)
		go tenantProviderSyncWorker(workerCtx, cfg, client, interval)
		logger.Info("tenant provider sync started (interval=%s)", interval)
	})
}

// StopTenantProviderSync stops the background tenant provider sync worker.
func StopTenantProviderSync() {
	if globalTenantProviderSync.cancel != nil {
		globalTenantProviderSync.cancel()
		globalTenantProviderSync.wg.Wait()
		globalTenantProviderSync.cancel = nil
	}
}

func tenantProviderSyncWorker(ctx context.Context, cfg *Config, client tenantProviderRuntimeUpdater, interval time.Duration) {
	defer globalTenantProviderSync.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	syncAllTenantProviders(ctx, cfg, client)

	for {
		select {
		case <-ticker.C:
			syncAllTenantProviders(ctx, cfg, client)
		case <-ctx.Done():
			return
		}
	}
}

func syncAllTenantProviders(ctx context.Context, cfg *Config, client tenantProviderRuntimeUpdater) {
	registry := cfg.TenantStore.Registry
	if registry == nil {
		return
	}
	if err := registry.SyncTenants(ctx); err != nil {
		logger.Warn("tenant provider sync: failed to refresh tenant registry: %v", err)
	}
	if cfg.TenantStore.LogStoreManager != nil {
		if err := cfg.TenantStore.LogStoreManager.SyncTenantsFromGlobalDB(ctx); err != nil {
			logger.Warn("tenant log store sync: failed to refresh tenant log stores: %v", err)
		}
	}

	tenantIDs := registry.ListTenantIDs(ctx)
	for _, tenantID := range tenantIDs {
		store := registry.GetStoreForTenant(ctx, tenantID)
		if store == nil {
			continue
		}
		if err := syncTenantProviders(ctx, cfg, client, tenantID, store); err != nil {
			logger.Debug("tenant provider sync failed for tenant %s: %v", tenantID, err)
			continue
		}
	}
}

func syncTenantProviders(
	ctx context.Context,
	cfg *Config,
	client tenantProviderRuntimeUpdater,
	tenantID string,
	store configstore.ConfigStore,
) error {
	refreshStartedAt := time.Now().UTC()
	since := configstore.NormalizeRefreshSince(cfg.tenantProviderRefreshWatermark(tenantID))

	if since.IsZero() {
		return syncTenantProvidersFull(ctx, cfg, client, tenantID, store, refreshStartedAt)
	}

	watermark := since.Add(-configstore.RefreshOverlap)
	delta, err := store.GetProviderConfigRefreshDelta(ctx, watermark)
	if err != nil {
		return err
	}
	if len(delta.Removed) > 0 {
		// Provider removal changes the tenant snapshot shape; fall back to full reload.
		return syncTenantProvidersFull(ctx, cfg, client, tenantID, store, refreshStartedAt)
	}
	if delta.IsEmpty() {
		cfg.setTenantProviderRefreshWatermark(tenantID, refreshStartedAt)
		return nil
	}

	validProviders := make(map[schemas.ModelProvider]configstore.ProviderConfig, len(delta.Changed))
	for provider, providerCfg := range delta.Changed {
		normalizeSyncedProviderConfig(&providerCfg)
		if err := ValidateCustomProvider(providerCfg, provider); err != nil {
			logger.Warn("tenant provider sync: skipping invalid provider %s for tenant %s: %v", provider, tenantID, err)
			continue
		}
		validProviders[provider] = providerCfg
		cfg.upsertTenantProviderEntry(tenantID, provider, providerCfg)
	}

	updated := cfg.mergeTenantProvidersIntoGlobal(tenantID, validProviders)
	for _, provider := range updated {
		if err := client.UpdateProvider(provider); err != nil {
			logger.Warn("tenant provider sync: failed to update runtime provider %s for tenant %s: %v", provider, tenantID, err)
		}
	}
	if len(validProviders) > 0 {
		logger.Debug("tenant provider sync: tenant %s applied %d changed provider(s)", tenantID, len(validProviders))
	}

	cfg.setTenantProviderRefreshWatermark(tenantID, refreshStartedAt)
	return nil
}

func syncTenantProvidersFull(
	ctx context.Context,
	cfg *Config,
	client tenantProviderRuntimeUpdater,
	tenantID string,
	store configstore.ConfigStore,
	refreshStartedAt time.Time,
) error {
	providers, err := store.GetProvidersConfig(ctx)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		cfg.setTenantProvidersSnapshot(tenantID, nil)
		cfg.setTenantProviderRefreshWatermark(tenantID, refreshStartedAt)
		return nil
	}

	validProviders := make(map[schemas.ModelProvider]configstore.ProviderConfig, len(providers))
	for provider, providerCfg := range providers {
		normalizeSyncedProviderConfig(&providerCfg)
		if err := ValidateCustomProvider(providerCfg, provider); err != nil {
			logger.Warn("tenant provider sync: skipping invalid provider %s for tenant %s: %v", provider, tenantID, err)
			continue
		}
		validProviders[provider] = providerCfg
	}

	cfg.setTenantProvidersSnapshot(tenantID, validProviders)
	updated := cfg.mergeTenantProvidersIntoGlobal(tenantID, validProviders)
	for _, provider := range updated {
		if err := client.UpdateProvider(provider); err != nil {
			logger.Warn("tenant provider sync: failed to update runtime provider %s for tenant %s: %v", provider, tenantID, err)
		}
	}
	if len(validProviders) > 0 {
		logger.Debug("tenant provider sync: tenant %s loaded %d provider(s)", tenantID, len(validProviders))
	}

	cfg.setTenantProviderRefreshWatermark(tenantID, refreshStartedAt)
	return nil
}

func normalizeSyncedProviderConfig(cfg *configstore.ProviderConfig) {
	if cfg == nil {
		return
	}
	if cfg.ConcurrencyAndBufferSize == nil || cfg.ConcurrencyAndBufferSize.Concurrency == 0 {
		defaultCBS := schemas.DefaultConcurrencyAndBufferSize
		cfg.ConcurrencyAndBufferSize = &defaultCBS
	}
	if cfg.NetworkConfig == nil {
		defaultNC := schemas.DefaultNetworkConfig
		cfg.NetworkConfig = &defaultNC
	}
}

func providerConfigEqual(a, b configstore.ProviderConfig) bool {
	hashA, errA := a.GenerateConfigHash("")
	hashB, errB := b.GenerateConfigHash("")
	if errA == nil && errB == nil && hashA != "" && hashA == hashB {
		return true
	}
	return reflect.DeepEqual(a, b)
}
