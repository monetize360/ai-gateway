package governance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTenantSnapshotLRUEvictsLeastRecentlyUsedTenant(t *testing.T) {
	cache := newTenantSnapshotLRU(2, time.Minute)
	cache.put("tenant-a", &redisGovernanceSnapshot{Version: 1})
	cache.put("tenant-b", &redisGovernanceSnapshot{Version: 2})
	_, ok := cache.get("tenant-a")
	require.True(t, ok)

	cache.put("tenant-c", &redisGovernanceSnapshot{Version: 3})
	_, ok = cache.get("tenant-b")
	require.False(t, ok)
	snapshot, ok := cache.get("tenant-a")
	require.True(t, ok)
	require.EqualValues(t, 1, snapshot.Version)
}

func TestTenantSnapshotLRUExpiresEntries(t *testing.T) {
	cache := newTenantSnapshotLRU(1, time.Millisecond)
	cache.put("tenant-a", &redisGovernanceSnapshot{Version: 1})
	time.Sleep(2 * time.Millisecond)

	_, ok := cache.get("tenant-a")
	require.False(t, ok)
}
