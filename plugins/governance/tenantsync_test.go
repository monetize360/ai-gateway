package governance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/valyala/fasthttp"
	"github.com/stretchr/testify/require"
)

type stubTenantGovernanceSyncSource struct {
	tenantIDs   []string
	stores      map[string]configstore.ConfigStore
	syncCalls   int
	listCalls   int
}

func (s *stubTenantGovernanceSyncSource) GetStoreFromContext(ctx context.Context) configstore.ConfigStore {
	return nil
}

func (s *stubTenantGovernanceSyncSource) GetStoreFromRequestCtx(_ *fasthttp.RequestCtx) configstore.ConfigStore {
	return nil
}

func (s *stubTenantGovernanceSyncSource) RequireStoreFromRequestCtx(_ *fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	return nil, fmt.Errorf("tenant context required")
}

func (s *stubTenantGovernanceSyncSource) ListTenantIDs(context.Context) []string {
	s.listCalls++
	return s.tenantIDs
}

func (s *stubTenantGovernanceSyncSource) SyncTenants(context.Context) error {
	s.syncCalls++
	return nil
}

func (s *stubTenantGovernanceSyncSource) GetStoreForTenant(_ context.Context, tenantID string) configstore.ConfigStore {
	return s.stores[tenantID]
}

func TestStartTenantGovernanceSyncRunsImmediatelyAndOnTicker(t *testing.T) {
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{
			*buildVirtualKey("vk1", "sk-bf-test", "Test VK", true),
		},
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := &stubTenantGovernanceSyncSource{
		tenantIDs: []string{"tenant-a"},
		stores:    map[string]configstore.ConfigStore{},
	}
	plugin := &GovernancePlugin{
		ctx:      ctx,
		logger:   logger,
		registry: source,
	}
	plugin.tenantComponents.Store("tenant-a", &tenantGovernanceComponents{store: store})

	plugin.StartTenantGovernanceSync(20 * time.Millisecond)

	require.Eventually(t, func() bool {
		return source.syncCalls >= 1 && source.listCalls >= 1
	}, time.Second, 5*time.Millisecond)

	cancel()
	if plugin.tenantSyncCancel != nil {
		plugin.tenantSyncCancel()
	}
}

func TestLocalGovernanceStoreRefreshFromDatabaseRequiresConfigStore(t *testing.T) {
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil)
	require.NoError(t, err)

	err = store.RefreshFromDatabase(context.Background())
	require.Error(t, err)
}
