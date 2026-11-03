package governance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTenantSnapshotLRUEvictsLeastRecentlyUsedTenant(t *testing.T) {
	cache := newTenantSnapshotLRU(2, time.Minute)
	cache.put("tenant-a", &redisGovernanceRaw{Version: 1})
	cache.put("tenant-b", &redisGovernanceRaw{Version: 2})
	_, ok := cache.get("tenant-a")
	require.True(t, ok)

	cache.put("tenant-c", &redisGovernanceRaw{Version: 3})
	_, ok = cache.get("tenant-b")
	require.False(t, ok)
	snapshot, ok := cache.get("tenant-a")
	require.True(t, ok)
	require.EqualValues(t, 1, snapshot.Version)
}

func TestTenantSnapshotLRUExpiresEntries(t *testing.T) {
	cache := newTenantSnapshotLRU(1, time.Millisecond)
	cache.put("tenant-a", &redisGovernanceRaw{Version: 1})
	time.Sleep(2 * time.Millisecond)

	_, ok := cache.get("tenant-a")
	require.False(t, ok)
}

func TestJoinEntityEnvelopes_JoinsDataPayloads(t *testing.T) {
	body, err := joinEntityEnvelopes("virtual_keys", []string{`{"data":{"id":"a"}}`, `{"data":{"id":"b"}}`})
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":"a"},{"id":"b"}]`, string(body))

	body, err = joinEntityEnvelopes("virtual_keys", nil)
	require.NoError(t, err)
	require.Equal(t, "[]", string(body))
}

func TestJoinEntityEnvelopes_RejectsPartialSnapshot(t *testing.T) {
	for name, bad := range map[string]string{
		"invalid json": `{"data":`,
		"missing data": `{"other":1}`,
		"null data":    `{"data":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := joinEntityEnvelopes("virtual_keys", []string{`{"data":{"id":"a"}}`, bad})
			require.Error(t, err, "an undecodable entry must fail the read, not be dropped")
		})
	}
}
