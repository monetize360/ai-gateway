package governance

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/tenantstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// virtualKeyCounter counts GetVirtualKey calls. That method is the Postgres
// preload behind the per-request transaction spike, so a hot-path hit must
// leave the counter at zero.
type virtualKeyCounter struct {
	configstore.ConfigStore
	mu   sync.Mutex
	gets int
	keys map[string]*configstoreTables.TableVirtualKey
}

func (s *virtualKeyCounter) GetVirtualKey(_ context.Context, id string) (*configstoreTables.TableVirtualKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	vk, ok := s.keys[id]
	if !ok || vk == nil {
		return nil, configstore.ErrNotFound
	}
	clone := *vk
	return &clone, nil
}

func (s *virtualKeyCounter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

type singleTenantRegistry struct {
	store configstore.ConfigStore
}

func (r *singleTenantRegistry) GetStoreFromContext(context.Context) configstore.ConfigStore {
	return r.store
}

func (r *singleTenantRegistry) GetStoreFromRequestCtx(*fasthttp.RequestCtx) configstore.ConfigStore {
	return r.store
}

func (r *singleTenantRegistry) RequireStoreFromRequestCtx(*fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	return r.store, nil
}

func (r *singleTenantRegistry) ListTenantIDs(context.Context) []string { return nil }

func (r *singleTenantRegistry) SyncTenants(context.Context) error { return nil }

func (r *singleTenantRegistry) GetStoreForTenant(context.Context, string) configstore.ConfigStore {
	return r.store
}

func (r *singleTenantRegistry) PeekStoreForTenant(string) configstore.ConfigStore { return r.store }

// scriptedGovernanceCache stands in for Redis. block, when set, holds load
// until closed so concurrent misses can be observed.
type scriptedGovernanceCache struct {
	mu        sync.Mutex
	loads     atomic.Int32
	snapshot  *redisGovernanceSnapshot
	err       error
	block     chan struct{}
	onChanged func(string, int64)
}

func (c *scriptedGovernanceCache) load(ctx context.Context, _ string) (*redisGovernanceSnapshot, error) {
	c.loads.Add(1)
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot, c.err
}

func (c *scriptedGovernanceCache) start(_ context.Context, onChanged func(string, int64)) {
	c.mu.Lock()
	c.onChanged = onChanged
	c.mu.Unlock()
}

func (c *scriptedGovernanceCache) stop() {}

func (c *scriptedGovernanceCache) publish(tenantID string, version int64) {
	c.mu.Lock()
	fn := c.onChanged
	c.mu.Unlock()
	if fn != nil {
		fn(tenantID, version)
	}
}

func (c *scriptedGovernanceCache) setSnapshot(snapshot *redisGovernanceSnapshot) {
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
}

func newHotPathPlugin(t *testing.T, memory []configstoreTables.TableVirtualKey, db *virtualKeyCounter, cache governanceSnapshotCache) (*GovernancePlugin, *LocalGovernanceStore) {
	t.Helper()
	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: memory,
	}, nil)
	require.NoError(t, err)
	plugin, err := InitFromStore(context.Background(), &Config{}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)
	plugin.registry = &singleTenantRegistry{store: db}
	plugin.redisCache = cache
	t.Cleanup(func() {
		_ = plugin.Cleanup()
	})
	return plugin, store
}

func TestResolveTenantVirtualKey_WarmMemorySkipsRedisAndPostgres(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "warm", true)
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{
		vk.ID: vk,
	}}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     1,
		VirtualKeys: []configstoreTables.TableVirtualKey{*buildVirtualKey(vk.ID, vk.Value, "from-redis", true)},
	}}
	plugin, _ := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, db, cache)

	first, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, second)

	assert.Equal(t, "warm", first.Name)
	assert.Equal(t, "warm", second.Name)
	assert.Equal(t, int32(0), cache.loads.Load(), "a warm tenant must not read Redis")
	assert.Equal(t, 0, db.count(), "a warm tenant must not call GetVirtualKey")
}

func TestResolveTenantVirtualKey_RedisHitSkipsPostgres(t *testing.T) {
	vk := buildVirtualKey("vk-redis", "vk-redis", "from-redis", true)
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{
		vk.ID: buildVirtualKey(vk.ID, vk.Value, "from-postgres", true),
	}}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     2,
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}}
	plugin, _ := newHotPathPlugin(t, nil, db, cache)

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	_, err = plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)

	assert.Equal(t, "from-redis", resolved.Name)
	assert.Equal(t, int32(1), cache.loads.Load())
	assert.Equal(t, 0, db.count(), "Redis hit must not call GetVirtualKey")
}

func TestResolveTenantVirtualKey_PostgresOnlyAfterRedisMiss(t *testing.T) {
	vk := buildVirtualKey("vk-db", "vk-db", "from-postgres", true)
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	cache := &scriptedGovernanceCache{}
	plugin, _ := newHotPathPlugin(t, nil, db, cache)

	first, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, second)

	assert.Equal(t, "from-postgres", first.Name)
	assert.Equal(t, int32(1), cache.loads.Load())
	assert.Equal(t, 1, db.count(), "Postgres is only the cold miss, and the row is kept in memory")
}

func TestResolveTenantVirtualKey_RedisErrorFallsBackToPostgresOnce(t *testing.T) {
	vk := buildVirtualKey("vk-db", "vk-db", "from-postgres", true)
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	cache := &scriptedGovernanceCache{err: assert.AnError}
	plugin, _ := newHotPathPlugin(t, nil, db, cache)

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	_, err = plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
	require.NoError(t, err)

	assert.Equal(t, 1, db.count())
}

func TestResolveTenantVirtualKey_ConcurrentRedisMissesCoalesce(t *testing.T) {
	vk := buildVirtualKey("vk-redis", "vk-redis", "from-redis", true)
	db := &virtualKeyCounter{}
	release := make(chan struct{})
	cache := &scriptedGovernanceCache{
		block: release,
		snapshot: &redisGovernanceSnapshot{
			Version:     3,
			VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		},
	}
	plugin, _ := newHotPathPlugin(t, nil, db, cache)

	const readers = 32
	errs := make(chan error, readers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resolved, resolveErr := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
			if resolveErr != nil {
				errs <- resolveErr
				return
			}
			if resolved == nil || resolved.Name != "from-redis" {
				errs <- assert.AnError
			}
		}()
	}
	close(start)

	require.Eventually(t, func() bool { return cache.loads.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), cache.loads.Load(), "waiters must join the in-flight Redis refresh")
	close(release)
	wg.Wait()
	close(errs)
	for resolveErr := range errs {
		require.NoError(t, resolveErr)
	}
	assert.Equal(t, int32(1), cache.loads.Load(), "one Redis refresh serves every concurrent miss")
	assert.Equal(t, 0, db.count())
}

func TestStartRedisGovernanceCache_UpdatesWarmSnapshotWithoutPostgres(t *testing.T) {
	stale := buildVirtualKey("vk-1", "vk-1", "stale", true)
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{
		stale.ID: buildVirtualKey(stale.ID, stale.Value, "from-postgres", true),
	}}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     1,
		VirtualKeys: []configstoreTables.TableVirtualKey{*stale},
	}}
	plugin, store := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*stale}, db, cache)
	plugin.StartRedisGovernanceCache()

	cache.setSnapshot(&redisGovernanceSnapshot{
		Version:     2,
		VirtualKeys: []configstoreTables.TableVirtualKey{*buildVirtualKey(stale.ID, stale.Value, "current", true)},
	})
	cache.publish(testTenantID, 2)

	require.Eventually(t, func() bool {
		vk, ok := store.GetVirtualKey(context.Background(), stale.ID)
		return ok && vk != nil && vk.Name == "current"
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, 0, db.count(), "a pub/sub refresh must not call GetVirtualKey")

	cache.publish("cold-tenant", 2)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), cache.loads.Load(), "a cold tenant stays cold until its first request")
}

var _ tenantstore.Resolver = (*singleTenantRegistry)(nil)
var _ governanceSnapshotCache = (*scriptedGovernanceCache)(nil)
