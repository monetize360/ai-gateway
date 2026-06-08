package lib

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/valyala/fasthttp"
	"github.com/stretchr/testify/require"
)

type stubProviderRuntime struct {
	updated []schemas.ModelProvider
}

func (s *stubProviderRuntime) UpdateProvider(provider schemas.ModelProvider) error {
	s.updated = append(s.updated, provider)
	return nil
}

func TestSyncTenantProvidersLoadsValidProviders(t *testing.T) {
	defaultCBS := schemas.DefaultConcurrencyAndBufferSize
	validCfg := configstore.ProviderConfig{
		ConcurrencyAndBufferSize: &defaultCBS,
		CustomProviderConfig: &schemas.CustomProviderConfig{
			IsKeyLess:        true,
			BaseProviderType: schemas.OpenAI,
			AllowedRequests: &schemas.AllowedRequests{
				ChatCompletion: schemas.Ptr(true),
			},
		},
	}
	invalidCfg := configstore.ProviderConfig{
		ConcurrencyAndBufferSize: &defaultCBS,
		CustomProviderConfig: &schemas.CustomProviderConfig{
			IsKeyLess: true,
		},
	}

	store := NewMockConfigStore()
	store.providers = map[schemas.ModelProvider]configstore.ProviderConfig{
		"fakellm":  validCfg,
		"brokenllm": invalidCfg,
	}

	cfg := &Config{Providers: make(map[schemas.ModelProvider]configstore.ProviderConfig)}
	runtime := &stubProviderRuntime{}

	err := syncTenantProviders(context.Background(), cfg, runtime, "tenant-a", store)
	require.NoError(t, err)
	require.Contains(t, cfg.Providers, schemas.ModelProvider("fakellm"))
	require.NotContains(t, cfg.Providers, schemas.ModelProvider("brokenllm"))
	require.Equal(t, []schemas.ModelProvider{"fakellm"}, runtime.updated)

	cfg.tenantProvidersMu.RLock()
	require.Contains(t, cfg.tenantProviders["tenant-a"], schemas.ModelProvider("fakellm"))
	cfg.tenantProvidersMu.RUnlock()
}

func TestConfiguredProviderNamesForTenantPrefersTenantSnapshot(t *testing.T) {
	cfg := &Config{
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			"openai": {},
		},
	}
	cfg.setTenantProvidersSnapshot("tenant-a", map[schemas.ModelProvider]configstore.ProviderConfig{
		"fakellm": {},
	})

	names := cfg.configuredProviderNamesForTenant("tenant-a")
	require.Equal(t, []schemas.ModelProvider{"fakellm"}, names)

	fallbackNames := cfg.configuredProviderNamesForTenant("")
	require.Equal(t, []schemas.ModelProvider{"openai"}, fallbackNames)
}

func TestStartTenantProviderSyncRunsOnStartup(t *testing.T) {
	source := &stubTenantProviderSyncSource{
		tenantIDs: []string{},
		stores:    map[string]*stubTenantProviderStore{},
	}
	cfg := &Config{
		Providers: make(map[schemas.ModelProvider]configstore.ProviderConfig),
		TenantStore: &TenantStoreHolder{
			Registry: source,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartTenantProviderSync(ctx, cfg, &stubProviderRuntime{}, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		return source.syncCalls >= 1
	}, time.Second, 5*time.Millisecond)

	StopTenantProviderSync()
}

type stubTenantProviderSyncSource struct {
	tenantIDs []string
	stores    map[string]*stubTenantProviderStore
	syncCalls int
}

type stubTenantProviderStore struct {
	providers map[schemas.ModelProvider]configstore.ProviderConfig
}

func (s *stubTenantProviderStore) GetProvidersConfig(context.Context) (map[schemas.ModelProvider]configstore.ProviderConfig, error) {
	return s.providers, nil
}

func (s *stubTenantProviderSyncSource) GetStoreFromContext(context.Context) configstore.ConfigStore {
	return nil
}

func (s *stubTenantProviderSyncSource) GetStoreFromRequestCtx(*fasthttp.RequestCtx) configstore.ConfigStore {
	return nil
}

func (s *stubTenantProviderSyncSource) RequireStoreFromRequestCtx(*fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	return nil, nil
}

func (s *stubTenantProviderSyncSource) ListTenantIDs(context.Context) []string {
	return s.tenantIDs
}

func (s *stubTenantProviderSyncSource) SyncTenants(context.Context) error {
	s.syncCalls++
	return nil
}

func (s *stubTenantProviderSyncSource) GetStoreForTenant(_ context.Context, tenantID string) configstore.ConfigStore {
	return s.stores[tenantID]
}
