package governance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// supplementalStore answers the database-only governance reads a Redis
// bootstrap makes, on top of the GetVirtualKey counter.
type supplementalStore struct {
	*virtualKeyCounter
	userOrgUnits []configstoreTables.TableUserOrgUnit
	reads        atomic.Int32

	spendMu      sync.Mutex
	budgetUsages []configstoreTables.TableBudgetUsage // full read result
	changed      []configstoreTables.TableBudgetUsage // delta read result
	fullReads    int
	deltaReads   int
	lastSince    time.Time
}

func (s *supplementalStore) GetBudgetUsages(context.Context) ([]configstoreTables.TableBudgetUsage, error) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.fullReads++
	return append([]configstoreTables.TableBudgetUsage(nil), s.budgetUsages...), nil
}

func (s *supplementalStore) GetBudgetUsagesChangedSince(_ context.Context, since time.Time) ([]configstoreTables.TableBudgetUsage, error) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.deltaReads++
	s.lastSince = since
	return append([]configstoreTables.TableBudgetUsage(nil), s.changed...), nil
}

func (s *supplementalStore) GetUserOrgUnits(context.Context) ([]configstoreTables.TableUserOrgUnit, error) {
	s.reads.Add(1)
	return s.userOrgUnits, nil
}
func (s *supplementalStore) DB() *gorm.DB { return nil }
func (s *supplementalStore) GetOrgAllowedModelConfigs(context.Context, []string) ([]configstoreTables.TableAllowedModelConfig, error) {
	return nil, nil
}
func (s *supplementalStore) GetOrgProviderAccess(context.Context, []string) ([]configstoreTables.TableProviderAccess, error) {
	return nil, nil
}
func (s *supplementalStore) GetOrgUnitAllowedModelConfigs(context.Context, []string) ([]configstoreTables.TableAllowedModelConfig, error) {
	return nil, nil
}
func (s *supplementalStore) GetOrgUnitProviderAccess(context.Context, []string) ([]configstoreTables.TableProviderAccess, error) {
	return nil, nil
}
func (s *supplementalStore) GetUserAllowedModelConfigs(context.Context, []string) ([]configstoreTables.TableAllowedModelConfig, error) {
	return nil, nil
}
func (s *supplementalStore) GetUserProviderAccess(context.Context, []string) ([]configstoreTables.TableProviderAccess, error) {
	return nil, nil
}
func (s *supplementalStore) GetAlertThresholds(context.Context) ([]configstoreTables.TableAlertThreshold, error) {
	return nil, nil
}
func (s *supplementalStore) GetAlertNotifications(context.Context) ([]configstoreTables.TableAlertNotification, error) {
	return nil, nil
}

type touchCountingRegistry struct {
	singleTenantRegistry
	touches atomic.Int32
}

func (r *touchCountingRegistry) TouchTenant(string) { r.touches.Add(1) }

// A burst of requests for a cold tenant (e.g. right after idle eviction
// released its components) must build the components once, not once per
// request: every extra build used to leak a running usage tracker.
func TestInitTenantGovernanceComponents_SingleFlight(t *testing.T) {
	vk := buildVirtualKey("vk-cold", "vk-cold", "from-redis", true)
	release := make(chan struct{})
	cache := &scriptedGovernanceCache{
		block:    release,
		snapshot: &redisGovernanceSnapshot{Version: 7, VirtualKeys: []configstoreTables.TableVirtualKey{*vk}},
	}
	db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	plugin, _ := newHotPathPlugin(t, nil, db.virtualKeyCounter, cache)
	plugin.registry = &singleTenantRegistry{store: db}

	const callers = 64
	comps := make(chan *tenantGovernanceComponents, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			comp, err := plugin.componentsForTenant(context.Background(), "cold-tenant")
			if err == nil {
				comps <- comp
			}
		}()
	}
	close(start)
	require.Eventually(t, func() bool { return cache.loads.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(comps)

	var first *tenantGovernanceComponents
	count := 0
	for comp := range comps {
		count++
		if first == nil {
			first = comp
		}
		assert.Same(t, first, comp, "every caller must get the single built instance")
	}
	assert.Equal(t, callers, count)
	assert.Equal(t, int32(1), cache.loads.Load(), "one bootstrap for the whole burst")
	assert.Equal(t, int32(1), db.reads.Load(), "database-only rows are loaded once")
	assert.EqualValues(t, 7, first.store.(*LocalGovernanceStore).RedisVersion())
}

// A request canceled while it leads the bootstrap must not fail the waiters.
func TestInitTenantGovernanceComponents_LeaderCancellationDoesNotAbortBuild(t *testing.T) {
	release := make(chan struct{})
	cache := &scriptedGovernanceCache{block: release, snapshot: &redisGovernanceSnapshot{Version: 1}}
	db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	plugin, _ := newHotPathPlugin(t, nil, db.virtualKeyCounter, cache)
	plugin.registry = &singleTenantRegistry{store: db}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = plugin.componentsForTenant(leaderCtx, "cold-tenant")
	}()
	require.Eventually(t, func() bool { return cache.loads.Load() == 1 }, time.Second, 5*time.Millisecond)
	cancel()
	close(release)
	<-leaderDone

	comp, err := plugin.componentsForTenant(context.Background(), "cold-tenant")
	require.NoError(t, err)
	require.NotNil(t, comp)
	assert.Equal(t, int32(1), cache.loads.Load())
}

func TestComponentsForTenant_WarmHitTouchesTenantPool(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "warm", true)
	db := &virtualKeyCounter{}
	plugin, _ := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, db, &scriptedGovernanceCache{})
	registry := &touchCountingRegistry{singleTenantRegistry: singleTenantRegistry{store: db}}
	plugin.registry = registry

	for range 3 {
		resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vk.ID)
		require.NoError(t, err)
		require.NotNil(t, resolved)
	}
	assert.Equal(t, int32(3), registry.touches.Load(), "hot-path use must count as pool activity")
}

// Repeated lookups of a missing key must not rebuild the store each time, and
// must not query Postgres on every request.
func TestResolveTenantVirtualKey_MissingKeyIsNegativelyCached(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "warm", true)
	db := &virtualKeyCounter{}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     4,
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}}
	plugin, store := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, db, cache)

	for range 5 {
		resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-missing")
		require.NoError(t, err)
		assert.Nil(t, resolved)
	}
	assert.Equal(t, 1, db.count(), "Postgres is asked once, then the miss is cached")
	assert.Equal(t, int32(1), cache.loads.Load())
	assert.EqualValues(t, 4, store.RedisVersion())

	// A newer Redis version invalidates the cached miss.
	cache.setSnapshot(&redisGovernanceSnapshot{Version: 5, VirtualKeys: []configstoreTables.TableVirtualKey{
		*vk, *buildVirtualKey("vk-missing", "vk-missing", "created", true),
	}})
	plugin.refreshTenantToVersion(testTenantID, plugin.loadTenantComponents(testTenantID), 5)
	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-missing")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "created", resolved.Name)
}

func TestRefreshTenantFromRedis_SameVersionSkipsRebuild(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "v1", true)
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{Version: 2, VirtualKeys: []configstoreTables.TableVirtualKey{*vk}}}
	plugin, store := newHotPathPlugin(t, nil, &virtualKeyCounter{}, cache)
	comp := plugin.loadTenantComponents(testTenantID)

	loaded, err := plugin.refreshTenantFromRedis(context.Background(), testTenantID, comp)
	require.NoError(t, err)
	require.True(t, loaded)
	before, ok := store.GetVirtualKey(context.Background(), vk.ID)
	require.True(t, ok)

	loaded, err = plugin.refreshTenantFromRedis(context.Background(), testTenantID, comp)
	require.NoError(t, err)
	require.True(t, loaded)
	after, ok := store.GetVirtualKey(context.Background(), vk.ID)
	require.True(t, ok)
	assert.Same(t, before, after, "an already-applied version must not be rebuilt")
}

func TestReconcileRedisVersions_AppliesMissedUpdate(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "stale", true)
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{Version: 1, VirtualKeys: []configstoreTables.TableVirtualKey{*vk}}}
	plugin, store := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, &virtualKeyCounter{}, cache)
	plugin.refreshTenantToVersion(testTenantID, plugin.loadTenantComponents(testTenantID), 1)

	// MPilot published version 2, but this instance never got the message.
	cache.setSnapshot(&redisGovernanceSnapshot{Version: 2, VirtualKeys: []configstoreTables.TableVirtualKey{
		*buildVirtualKey(vk.ID, vk.Value, "current", false),
	}})
	plugin.reconcileRedisVersions()

	current, ok := store.GetVirtualKey(context.Background(), vk.ID)
	require.True(t, ok)
	assert.Equal(t, "current", current.Name)
	assert.EqualValues(t, 2, store.RedisVersion())
}

func TestReplaceFromRedisSnapshot_PreservesLocalCountersAndDatabaseOnlyRows(t *testing.T) {
	limit := int64(1000)
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), nil, nil)
	require.NoError(t, err)
	store.userOrgUnits.Store("mapping-1", &configstoreTables.TableUserOrgUnit{ID: "mapping-1"})
	store.ReplaceFromRedisSnapshot(context.Background(), &redisGovernanceSnapshot{
		Version:    1,
		RateLimits: []configstoreTables.TableRateLimit{{ID: "rl-1", TokenMaxLimit: &limit}},
	}, nil)

	raw, ok := store.rateLimits.Load("rl-1")
	require.True(t, ok)
	raw.(*configstoreTables.TableRateLimit).TokenCurrentUsage = 42
	raw.(*configstoreTables.TableRateLimit).RequestCurrentUsage = 7

	store.ReplaceFromRedisSnapshot(context.Background(), &redisGovernanceSnapshot{
		Version:    2,
		RateLimits: []configstoreTables.TableRateLimit{{ID: "rl-1", TokenMaxLimit: &limit, TokenCurrentUsage: 0}},
	}, nil)

	raw, ok = store.rateLimits.Load("rl-1")
	require.True(t, ok)
	assert.EqualValues(t, 42, raw.(*configstoreTables.TableRateLimit).TokenCurrentUsage, "local counters survive a Redis refresh")
	assert.EqualValues(t, 7, raw.(*configstoreTables.TableRateLimit).RequestCurrentUsage)
	_, ok = store.userOrgUnits.Load("mapping-1")
	assert.True(t, ok, "rows Redis does not carry must not be wiped")
}

func TestSweepStaleSyncMap_KeepsRestoredAndDropsStale(t *testing.T) {
	var m sync.Map
	kept, dropped := &configstoreTables.TableBudget{ID: "a"}, &configstoreTables.TableBudget{ID: "b"}
	rules := []*configstoreTables.TableRoutingRule{{ID: "r"}}
	m.Store("a", kept)
	m.Store("b", dropped)
	m.Store("rules", rules)
	before := syncMapContents(&m)

	m.Store("a", &configstoreTables.TableBudget{ID: "a"})              // re-stored by the rebuild
	m.Store("rules", []*configstoreTables.TableRoutingRule{{ID: "r"}}) // slices must not panic
	sweepStaleSyncMap(&m, before)

	_, ok := m.Load("a")
	assert.True(t, ok)
	_, ok = m.Load("rules")
	assert.True(t, ok)
	_, ok = m.Load("b")
	assert.False(t, ok, "keys absent from the new view are removed")
}

func TestUsageTrackerStop_IsIdempotentAndDoesNotDump(t *testing.T) {
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), nil, nil)
	require.NoError(t, err)
	tracker := NewUsageTracker(context.Background(), store, nil, nil, NewMockLogger())
	tracker.Stop()
	tracker.Stop()
	require.NoError(t, tracker.Cleanup())
}

func budgetUsageSpend(t *testing.T, store *LocalGovernanceStore, id string) (float64, bool) {
	t.Helper()
	raw, ok := store.budgetUsages.Load(id)
	if !ok {
		return 0, false
	}
	return raw.(*configstoreTables.TableBudgetUsage).CurrentUsage, true
}

// Spend written to Postgres by MPilot Rating must reach the gateway even when
// MPilot never bumps the Redis version.
func TestRefreshBudgetUsagesFromDatabase_FullThenDelta(t *testing.T) {
	db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	db.budgetUsages = []configstoreTables.TableBudgetUsage{
		{ID: "bu-1", CurrentUsage: 10},
		{ID: "bu-2", CurrentUsage: 20},
	}
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), db, nil)
	require.NoError(t, err)
	store.budgetUsages.Store("bu-stale", &configstoreTables.TableBudgetUsage{ID: "bu-stale"})

	// No watermark yet: one full read replaces the map.
	require.NoError(t, store.RefreshBudgetUsagesFromDatabase(context.Background()))
	spend, ok := budgetUsageSpend(t, store, "bu-1")
	require.True(t, ok)
	assert.EqualValues(t, 10, spend)
	_, ok = budgetUsageSpend(t, store, "bu-stale")
	assert.False(t, ok, "rows absent from the full read are removed")
	assert.Equal(t, 1, db.fullReads)
	firstWatermark := store.spendWatermark
	require.False(t, firstWatermark.IsZero())

	// Rating priced more requests: bu-1 grew, bu-2 was deleted.
	db.changed = []configstoreTables.TableBudgetUsage{
		{ID: "bu-1", CurrentUsage: 95},
		{ID: "bu-2", SystemColumns: configstoreTables.SystemColumns{Deleted: true}},
	}
	require.NoError(t, store.RefreshBudgetUsagesFromDatabase(context.Background()))
	spend, ok = budgetUsageSpend(t, store, "bu-1")
	require.True(t, ok)
	assert.EqualValues(t, 95, spend, "new spend from the database is applied")
	_, ok = budgetUsageSpend(t, store, "bu-2")
	assert.False(t, ok, "deleted rows are dropped")
	assert.Equal(t, 1, db.fullReads, "after the first read only changed rows are fetched")
	assert.Equal(t, 1, db.deltaReads)
	assert.True(t, db.lastSince.Before(firstWatermark), "the delta window overlaps the previous read")
	assert.True(t, store.spendWatermark.After(firstWatermark) || store.spendWatermark.Equal(firstWatermark))
}

// Redis may hold stale spend; a Redis apply must take spend from the database.
func TestReplaceFromRedisSnapshot_TakesSpendFromDatabase(t *testing.T) {
	db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	db.budgetUsages = []configstoreTables.TableBudgetUsage{{ID: "bu-1", CurrentUsage: 250}}
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), db, nil)
	require.NoError(t, err)

	supplemental, err := store.loadSupplementalFromDatabase(context.Background())
	require.NoError(t, err)
	store.ReplaceFromRedisSnapshot(context.Background(), &redisGovernanceSnapshot{
		Version:      3,
		BudgetUsages: []configstoreTables.TableBudgetUsage{{ID: "bu-1", CurrentUsage: 40}},
	}, supplemental)

	spend, ok := budgetUsageSpend(t, store, "bu-1")
	require.True(t, ok)
	assert.EqualValues(t, 250, spend, "database spend wins over the Redis copy")
	assert.Equal(t, supplemental.readStartedAt, store.spendWatermark)
}

// Without database rows (no configStore), a Redis apply keeps Redis spend and
// clears the watermark so the next spend refresh does a full read.
func TestReplaceFromRedisSnapshot_WithoutDatabaseResetsSpendWatermark(t *testing.T) {
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), nil, nil)
	require.NoError(t, err)
	store.spendWatermark = time.Now()
	store.ReplaceFromRedisSnapshot(context.Background(), &redisGovernanceSnapshot{
		Version:      1,
		BudgetUsages: []configstoreTables.TableBudgetUsage{{ID: "bu-1", CurrentUsage: 40}},
	}, nil)
	spend, ok := budgetUsageSpend(t, store, "bu-1")
	require.True(t, ok)
	assert.EqualValues(t, 40, spend)
	assert.True(t, store.spendWatermark.IsZero())
}

func TestRefreshAllTenantSpend_RefreshesEveryWarmTenant(t *testing.T) {
	plugin, _ := newHotPathPlugin(t, nil, &virtualKeyCounter{}, &scriptedGovernanceCache{})
	var dbs []*supplementalStore
	for _, tenantID := range []string{"tenant-a", "tenant-b", "tenant-c"} {
		db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
		db.budgetUsages = []configstoreTables.TableBudgetUsage{{ID: "bu-" + tenantID, CurrentUsage: 7}}
		store, err := newEmptyLocalGovernanceStore(NewMockLogger(), db, nil)
		require.NoError(t, err)
		plugin.tenantComponents.Store(tenantID, &tenantGovernanceComponents{store: store})
		dbs = append(dbs, db)
	}

	plugin.refreshAllTenantSpend()

	for _, db := range dbs {
		assert.Equal(t, 1, db.fullReads)
	}
	raw, ok := plugin.tenantComponents.Load("tenant-b")
	require.True(t, ok)
	spend, ok := budgetUsageSpend(t, raw.(*tenantGovernanceComponents).store.(*LocalGovernanceStore), "bu-tenant-b")
	require.True(t, ok)
	assert.EqualValues(t, 7, spend)
}

// A tenant this pod has not served since its last spend refresh is skipped;
// serving it again makes the next refresh include it.
func TestRefreshAllTenantSpend_SkipsTenantsNotServedSinceLastRefresh(t *testing.T) {
	plugin, _ := newHotPathPlugin(t, nil, &virtualKeyCounter{}, &scriptedGovernanceCache{})
	db := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	db.budgetUsages = []configstoreTables.TableBudgetUsage{{ID: "bu-1", CurrentUsage: 1}}
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), db, nil)
	require.NoError(t, err)
	comp := &tenantGovernanceComponents{store: store}
	plugin.tenantComponents.Store("tenant-a", comp)

	plugin.refreshAllTenantSpend() // never refreshed: always included
	assert.Equal(t, 1, db.fullReads)
	plugin.refreshAllTenantSpend() // not served since: skipped
	assert.Equal(t, 0, db.deltaReads)

	comp.markServed()
	plugin.refreshAllTenantSpend()
	assert.Equal(t, 1, db.deltaReads, "a served tenant is refreshed on the next tick")
	plugin.refreshAllTenantSpend()
	assert.Equal(t, 1, db.deltaReads)

	comp.markServed() // immediately after a refresh: must still register
	plugin.refreshAllTenantSpend()
	assert.Equal(t, 2, db.deltaReads)
}

// replacedPoolRegistry hands out stores in order from GetStoreForTenant and
// reports current as the tenant's open pool, so a test can simulate the pool
// being closed or replaced while a governance bootstrap is running.
type replacedPoolRegistry struct {
	singleTenantRegistry
	mu      sync.Mutex
	stores  []configstore.ConfigStore
	gets    int
	current configstore.ConfigStore
}

func (r *replacedPoolRegistry) GetStoreForTenant(context.Context, string) configstore.ConfigStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gets >= len(r.stores) {
		return nil
	}
	store := r.stores[r.gets]
	r.gets++
	return store
}

func (r *replacedPoolRegistry) PeekStoreForTenant(string) configstore.ConfigStore { return r.current }

func coldTenantPlugin(t *testing.T) *GovernancePlugin {
	t.Helper()
	vk := buildVirtualKey("vk-cold", "vk-cold", "from-redis", true)
	cache := &scriptedGovernanceCache{
		snapshot: &redisGovernanceSnapshot{Version: 3, VirtualKeys: []configstoreTables.TableVirtualKey{*vk}},
	}
	plugin, _ := newHotPathPlugin(t, nil, &virtualKeyCounter{}, cache)
	return plugin
}

// D1: a pool replaced during the bootstrap must not leave the tenant served
// from the closed pool. The build is retried against the current pool.
func TestInitTenantGovernanceComponents_PoolReplacedDuringBuildRebindsToCurrentPool(t *testing.T) {
	closed := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	current := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	plugin := coldTenantPlugin(t)
	registry := &replacedPoolRegistry{stores: []configstore.ConfigStore{closed, current}, current: current}
	plugin.registry = registry

	comp, err := plugin.componentsForTenant(context.Background(), "cold-tenant")
	require.NoError(t, err)
	require.NotNil(t, comp)
	assert.Same(t, current, comp.store.(*LocalGovernanceStore).configStore, "components must be bound to the open pool")
	assert.Same(t, comp, plugin.loadTenantComponents("cold-tenant"))
	assert.Equal(t, 2, registry.gets, "one retry against the current pool")
}

// D1: if the tenant has no open pool any more (e.g. soft-deleted), nothing is
// cached, so the next request starts clean instead of reusing a closed pool.
func TestInitTenantGovernanceComponents_PoolClosedDuringBuildCachesNothing(t *testing.T) {
	closed := &supplementalStore{virtualKeyCounter: &virtualKeyCounter{}}
	plugin := coldTenantPlugin(t)
	plugin.registry = &replacedPoolRegistry{stores: []configstore.ConfigStore{closed}, current: nil}

	comp, err := plugin.componentsForTenant(context.Background(), "cold-tenant")
	assert.Error(t, err)
	assert.Nil(t, comp)
	assert.Nil(t, plugin.loadTenantComponents("cold-tenant"), "components bound to a closed pool must not be cached")
}

// D2: increments made while a Redis rebuild is running must survive it.
func TestReplaceFromRedisSnapshot_KeepsIncrementsMadeDuringRebuild(t *testing.T) {
	ctx := context.Background()
	store, err := newEmptyLocalGovernanceStore(NewMockLogger(), nil, nil)
	require.NoError(t, err)
	limit := int64(1 << 40)
	// Enough virtual keys that each rebuild spends real time before it
	// reaches the rate limits, which is where increments used to be lost.
	snapshot := func(version int64) *redisGovernanceSnapshot {
		s := &redisGovernanceSnapshot{
			Version:    version,
			RateLimits: []configstoreTables.TableRateLimit{{ID: "rl-1", TokenMaxLimit: &limit, RequestMaxLimit: &limit}},
		}
		for i := range 300 {
			id := fmt.Sprintf("vk-%d", i)
			s.VirtualKeys = append(s.VirtualKeys, *buildVirtualKey(id, id, id, true))
		}
		return s
	}
	store.ReplaceFromRedisSnapshot(ctx, snapshot(1), nil)

	const workers, bumpsPerWorker, tokensPerBump = 4, 5000, 3
	stop := make(chan struct{})
	var rebuilds atomic.Int32
	rebuilderDone := make(chan struct{})
	go func() {
		defer close(rebuilderDone)
		for version := int64(2); ; version++ {
			select {
			case <-stop:
				return
			default:
			}
			store.ReplaceFromRedisSnapshot(ctx, snapshot(version), nil)
			rebuilds.Add(1)
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range bumpsPerWorker {
				require.NoError(t, store.BumpRateLimitUsage(ctx, "rl-1", tokensPerBump, true, true))
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-rebuilderDone

	require.Positive(t, rebuilds.Load(), "rebuilds must overlap the increments")
	raw, ok := store.rateLimits.Load("rl-1")
	require.True(t, ok)
	rl := raw.(*configstoreTables.TableRateLimit)
	assert.EqualValues(t, workers*bumpsPerWorker, rl.RequestCurrentUsage, "no request increment may be lost (rebuilds=%d)", rebuilds.Load())
	assert.EqualValues(t, workers*bumpsPerWorker*tokensPerBump, rl.TokenCurrentUsage, "no token increment may be lost")
}

// D3: without a Redis snapshot no version change can announce a new key, so a
// cached miss must expire quickly instead of rejecting the key for 10s.
func TestResolveTenantVirtualKey_MissWithoutRedisSnapshotExpiresQuickly(t *testing.T) {
	db := &virtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{}}
	plugin, store := newHotPathPlugin(t, nil, db, &scriptedGovernanceCache{})
	require.EqualValues(t, 0, store.RedisVersion())

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-new")
	require.NoError(t, err)
	require.Nil(t, resolved)

	db.mu.Lock()
	db.keys["vk-new"] = buildVirtualKey("vk-new", "vk-new", "created", true)
	db.mu.Unlock()

	resolved, err = plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-new")
	require.NoError(t, err)
	assert.Nil(t, resolved, "the miss is still cached briefly to protect Postgres")

	require.Eventually(t, func() bool {
		vk, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-new")
		return err == nil && vk != nil
	}, 2*time.Second, 50*time.Millisecond, "a new key must be accepted within about a second")
}

// resolveEventually waits until vkID resolves, failing after limit.
func resolveEventually(t *testing.T, plugin *GovernancePlugin, vkID string, limit time.Duration, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		vk, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, vkID)
		return err == nil && vk != nil
	}, limit, 50*time.Millisecond, msg)
}

func addVirtualKey(db *virtualKeyCounter, id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.keys == nil {
		db.keys = map[string]*configstoreTables.TableVirtualKey{}
	}
	db.keys[id] = buildVirtualKey(id, id, "created", true)
}

// F10: a namespace that was applied and then vanished (flush, eviction,
// failover to an empty replica, deletion) leaves the applied version set, but
// Redis can no longer announce new keys, so the short cache time must apply.
func TestResolveTenantVirtualKey_MissAfterRedisNamespaceLostExpiresQuickly(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "warm", true)
	db := &virtualKeyCounter{}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     4,
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}}
	plugin, store := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, db, cache)
	plugin.refreshTenantToVersion(testTenantID, plugin.loadTenantComponents(testTenantID), 4)
	require.EqualValues(t, 4, store.RedisVersion(), "the namespace was applied")

	cache.setSnapshot(nil) // namespace lost; the applied version stays 4

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-new")
	require.NoError(t, err)
	require.Nil(t, resolved)
	require.EqualValues(t, 4, store.RedisVersion())

	addVirtualKey(db, "vk-new")
	resolveEventually(t, plugin, "vk-new", 2*time.Second, "a key created after the namespace was lost must be accepted within about a second")
}

// While Redis is unreachable nothing can announce a new key either.
func TestResolveTenantVirtualKey_MissWhileRedisUnavailableExpiresQuickly(t *testing.T) {
	db := &virtualKeyCounter{}
	cache := &scriptedGovernanceCache{err: errors.New("redis: connection refused")}
	plugin, _ := newHotPathPlugin(t, nil, db, cache)

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-new")
	require.NoError(t, err)
	require.Nil(t, resolved)

	addVirtualKey(db, "vk-new")
	resolveEventually(t, plugin, "vk-new", 2*time.Second, "a key created during a Redis outage must be accepted within about a second")
}

// Guard: with a live snapshot a version bump announces new keys, so the miss
// keeps the long cache time and Postgres is not asked again after a second.
func TestResolveTenantVirtualKey_MissWithLiveRedisSnapshotKeepsLongCache(t *testing.T) {
	vk := buildVirtualKey("vk-1", "vk-1", "warm", true)
	db := &virtualKeyCounter{}
	cache := &scriptedGovernanceCache{snapshot: &redisGovernanceSnapshot{
		Version:     4,
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}}
	plugin, _ := newHotPathPlugin(t, []configstoreTables.TableVirtualKey{*vk}, db, cache)

	resolved, err := plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-missing")
	require.NoError(t, err)
	require.Nil(t, resolved)
	require.Equal(t, 1, db.count())

	time.Sleep(vkMissTTLWithoutRedis + 200*time.Millisecond)
	resolved, err = plugin.ResolveTenantVirtualKey(context.Background(), testTenantID, "vk-missing")
	require.NoError(t, err)
	assert.Nil(t, resolved)
	assert.Equal(t, 1, db.count(), "the miss is still cached past the short TTL")
}
