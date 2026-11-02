package governance

import (
	"context"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTenantVirtualKey_UsesWarmInMemoryStore(t *testing.T) {
	logger := NewMockLogger()
	vk := buildVirtualKey("vk-1", "vk-1", "Test VK", true)
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, vk.ID, resolved.ID)
	assert.Equal(t, vk.Name, resolved.Name)
}

func TestResolveTenantVirtualKey_WarmCacheSupportsConcurrentReads(t *testing.T) {
	logger := NewMockLogger()
	vk := buildVirtualKey("vk-1", "vk-1", "Test VK", true)
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	const readers = 64
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolved, resolveErr := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
			if resolveErr != nil {
				errs <- resolveErr
				return
			}
			if resolved == nil || resolved.ID != vk.ID {
				errs <- assert.AnError
			}
		}()
	}
	wg.Wait()
	close(errs)
	for resolveErr := range errs {
		require.NoError(t, resolveErr)
	}
}

func TestResolveTenantVirtualKey_MissingKeyNeedsFallbackStore(t *testing.T) {
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	_, err = plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenant store unavailable")
}
