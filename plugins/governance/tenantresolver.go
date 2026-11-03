package governance

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

const (
	// vkMissTTL bounds how long a confirmed-missing virtual key is answered from
	// memory. A Redis version change invalidates the entry sooner.
	vkMissTTL = 10 * time.Second
	// vkMissTTLWithoutRedis applies when Redis has no snapshot for the tenant
	// at lookup time (never seeded, namespace lost, or Redis unreachable). No
	// version change can then announce a newly created key, so the TTL alone
	// decides how long that key is rejected. It still caps Postgres lookups for
	// a repeatedly-missing key at about one per second.
	vkMissTTLWithoutRedis = 1 * time.Second
	// tenantRefreshTimeout bounds one shared Redis refresh. It is detached from
	// the request that started it because other requests wait on the result.
	tenantRefreshTimeout = 10 * time.Second
)

type vkMissEntry struct {
	expires      time.Time
	redisVersion int64
}

// tenantActivityToucher is implemented by registries whose idle-pool eviction
// must see hot-path use. Inference served entirely from memory never opens the
// tenant pool, so without this the pool (and the tenant's governance
// components) would be evicted as idle while under load.
type tenantActivityToucher interface {
	TouchTenant(tenantID string)
}

// ResolveTenantVirtualKey resolves an inference JWT virtual key without doing a
// database query on cache hits. It uses the tenant's governance memory snapshot
// first, then rehydrates that snapshot from Redis, and only then falls back to
// Postgres when Redis cannot satisfy the lookup.
func (p *GovernancePlugin) ResolveTenantVirtualKey(ctx context.Context, tenantID, virtualKeyID string) (*configstoreTables.TableVirtualKey, error) {
	if p == nil {
		return nil, fmt.Errorf("governance plugin is not configured")
	}
	tenantID = strings.TrimSpace(tenantID)
	virtualKeyID = strings.TrimSpace(virtualKeyID)
	if tenantID == "" || virtualKeyID == "" {
		return nil, nil
	}

	comp, err := p.componentsForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if vk, ok := comp.store.GetVirtualKey(ctx, virtualKeyID); ok {
		return vk, nil
	}
	missKey := tenantID + "\x00" + virtualKeyID
	if p.isKnownVKMiss(missKey, comp) {
		return nil, nil
	}

	loadedFromRedis, err := p.refreshTenantFromRedis(ctx, tenantID, comp)
	if err != nil {
		p.logger.Warn("failed to refresh governance cache for tenant %s: %v", tenantID, err)
	}
	if loadedFromRedis {
		if vk, ok := comp.store.GetVirtualKey(ctx, virtualKeyID); ok {
			return vk, nil
		}
	}

	// Redis is either unavailable, not seeded, or does not yet contain this
	// newly-created key. Postgres remains the bootstrap and recovery source.
	if p.registry == nil {
		return nil, fmt.Errorf("tenant store unavailable")
	}
	store := p.registry.GetStoreForTenant(ctx, tenantID)
	if store == nil {
		return nil, fmt.Errorf("tenant store unavailable")
	}
	vk, err := store.GetVirtualKey(ctx, virtualKeyID)
	if err != nil {
		if err == configstore.ErrNotFound {
			p.rememberVKMiss(missKey, comp, loadedFromRedis)
			return nil, nil
		}
		return nil, err
	}
	comp.store.UpdateVirtualKeyInMemory(ctx, vk, nil, nil, nil)
	return vk, nil
}

func (p *GovernancePlugin) componentsForTenant(ctx context.Context, tenantID string) (*tenantGovernanceComponents, error) {
	if raw, ok := p.tenantComponents.Load(tenantID); ok {
		if comp, ok := raw.(*tenantGovernanceComponents); ok && comp != nil && comp.store != nil {
			p.touchTenant(tenantID)
			comp.markServed()
			return comp, nil
		}
	}
	if p.registry == nil {
		return nil, fmt.Errorf("tenant governance store is not initialized")
	}
	store := p.registry.GetStoreForTenant(ctx, tenantID)
	if store == nil {
		return nil, fmt.Errorf("tenant store unavailable")
	}
	comp := p.initTenantGovernanceComponents(ctx, tenantID, store)
	if comp == nil || comp.store == nil {
		return nil, fmt.Errorf("tenant governance store is not initialized")
	}
	comp.markServed()
	return comp, nil
}

func (p *GovernancePlugin) touchTenant(tenantID string) {
	if toucher, ok := p.registry.(tenantActivityToucher); ok {
		toucher.TouchTenant(tenantID)
	}
}

func storeRedisVersion(comp *tenantGovernanceComponents) int64 {
	if comp == nil {
		return 0
	}
	if localStore, ok := comp.store.(*LocalGovernanceStore); ok {
		return localStore.RedisVersion()
	}
	return 0
}

func (p *GovernancePlugin) isKnownVKMiss(key string, comp *tenantGovernanceComponents) bool {
	raw, ok := p.vkMisses.Load(key)
	if !ok {
		return false
	}
	entry := raw.(vkMissEntry)
	if time.Now().After(entry.expires) || entry.redisVersion != storeRedisVersion(comp) {
		p.vkMisses.CompareAndDelete(key, raw)
		return false
	}
	return true
}

// rememberVKMiss caches a confirmed-missing key. redisBacked reports whether
// Redis served a snapshot for the tenant on this lookup. Only then can a later
// version bump announce the key and clear the entry early. The last applied
// version is not enough: a namespace that was applied and has since vanished
// (flush, eviction, failover to an empty replica, deletion) leaves it set.
func (p *GovernancePlugin) rememberVKMiss(key string, comp *tenantGovernanceComponents, redisBacked bool) {
	ttl := vkMissTTL
	if !redisBacked {
		ttl = vkMissTTLWithoutRedis
	}
	p.vkMisses.Store(key, vkMissEntry{expires: time.Now().Add(ttl), redisVersion: storeRedisVersion(comp)})
}

// sweepVKMisses drops expired negative-cache entries.
func (p *GovernancePlugin) sweepVKMisses() {
	now := time.Now()
	p.vkMisses.Range(func(key, value any) bool {
		if entry, ok := value.(vkMissEntry); !ok || now.After(entry.expires) {
			p.vkMisses.CompareAndDelete(key, value)
		}
		return true
	})
}

// refreshTenantFromRedis coalesces refreshes per tenant. It reports whether a
// Redis snapshot is (now) applied. A snapshot whose version is already applied
// is not rebuilt again, so repeated misses cost one Redis GET, not a rebuild.
func (p *GovernancePlugin) refreshTenantFromRedis(ctx context.Context, tenantID string, comp *tenantGovernanceComponents) (bool, error) {
	if p.redisCache == nil || comp == nil {
		return false, nil
	}
	localStore, ok := comp.store.(*LocalGovernanceStore)
	if !ok {
		return false, nil
	}

	call := &tenantRefreshCall{done: make(chan struct{})}
	actual, loaded := p.tenantRefreshes.LoadOrStore(tenantID, call)
	if loaded {
		inFlight := actual.(*tenantRefreshCall)
		select {
		case <-inFlight.done:
			return inFlight.loaded, inFlight.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	defer func() {
		close(call.done)
		p.tenantRefreshes.CompareAndDelete(tenantID, call)
	}()

	parent := p.ctx
	if parent == nil {
		parent = context.Background()
	}
	refreshCtx, cancel := context.WithTimeout(parent, tenantRefreshTimeout)
	defer cancel()

	snapshot, err := p.redisCache.load(refreshCtx, tenantID)
	if err != nil || snapshot == nil {
		call.err = err
		return false, err
	}
	if snapshot.Version != localStore.RedisVersion() {
		supplemental, supErr := localStore.loadSupplementalFromDatabase(refreshCtx)
		if supErr != nil {
			// Keep serving the current view rather than one with access-control
			// rows wiped; the next refresh retries.
			call.err = supErr
			return false, supErr
		}
		localStore.ReplaceFromRedisSnapshot(refreshCtx, snapshot, supplemental)
	}
	call.loaded = true
	return true, nil
}

// refreshTenantToVersion refreshes until the store has applied at least
// version. A refresh that was already in flight when MPilot published may
// return the previous snapshot; retrying keeps that update from being lost.
func (p *GovernancePlugin) refreshTenantToVersion(tenantID string, comp *tenantGovernanceComponents, version int64) {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		loaded, err := p.refreshTenantFromRedis(p.ctx, tenantID, comp)
		if err != nil {
			p.logger.Warn("failed to apply Redis governance update for tenant %s: %v", tenantID, err)
		} else if !loaded {
			p.logger.Warn("Redis governance update for tenant %s has no snapshot", tenantID)
			return
		} else if version <= 0 || storeRedisVersion(comp) >= version {
			return
		}
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
}

// reconcileRedisVersions compares every warm tenant's applied version with
// Redis and refreshes the ones that are behind. It is the safety net for
// pub/sub messages lost during a reconnect or failover.
func (p *GovernancePlugin) reconcileRedisVersions() {
	p.sweepVKMisses()
	if p.redisCache == nil {
		return
	}
	comps := make(map[string]*tenantGovernanceComponents)
	p.tenantComponents.Range(func(key, value any) bool {
		tenantID, ok := key.(string)
		comp, ok2 := value.(*tenantGovernanceComponents)
		if ok && ok2 && comp != nil {
			comps[tenantID] = comp
		}
		return true
	})
	if len(comps) == 0 {
		return
	}
	tenantIDs := make([]string, 0, len(comps))
	for tenantID := range comps {
		tenantIDs = append(tenantIDs, tenantID)
	}
	ctx, cancel := context.WithTimeout(p.ctx, tenantRefreshTimeout)
	versions, err := p.redisCache.versions(ctx, tenantIDs)
	cancel()
	if err != nil {
		p.logger.Warn("governance Redis reconcile failed: %v", err)
		return
	}
	for tenantID, remote := range versions {
		comp := comps[tenantID]
		if remote <= 0 || remote == storeRedisVersion(comp) {
			continue
		}
		p.logger.Info("governance Redis reconcile: tenant %s behind (local=%d, redis=%d)", tenantID, storeRedisVersion(comp), remote)
		p.refreshTenantToVersion(tenantID, comp, remote)
	}
}

const (
	// spendRefreshConcurrency bounds how many tenant databases one pod queries
	// at the same time during a spend refresh.
	spendRefreshConcurrency = 8
	spendRefreshTimeout     = 10 * time.Second
)

// StartTenantSpendRefresh periodically reads BudgetUsage spend back from each
// warm tenant's database. MPilot Rating writes spend to Postgres and the
// gateway never adds it itself, so without this the in-memory spend would
// stay at whatever was loaded and budgets would never block. It does not
// depend on MPilot bumping the Redis version after rating.
//
// The refresh uses each tenant's already-open store directly, so it does not
// count as pool activity: an idle tenant is still evicted and released.
func (p *GovernancePlugin) StartTenantSpendRefresh(interval time.Duration) {
	if p == nil || interval <= 0 {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-ticker.C:
				p.refreshAllTenantSpend()
			}
		}
	}()
	p.logger.Info("tenant budget spend refresh started (interval=%s)", interval)
}

// refreshAllTenantSpend runs one spend refresh for every warm tenant.
func (p *GovernancePlugin) refreshAllTenantSpend() {
	type target struct {
		tenantID string
		comp     *tenantGovernanceComponents
		store    *LocalGovernanceStore
	}
	var targets []target
	p.tenantComponents.Range(func(key, value any) bool {
		tenantID, ok := key.(string)
		comp, ok2 := value.(*tenantGovernanceComponents)
		if !ok || !ok2 || comp == nil || !comp.needsSpendRefresh() {
			return true
		}
		if store, ok := comp.store.(*LocalGovernanceStore); ok && store.configStore != nil {
			targets = append(targets, target{tenantID: tenantID, comp: comp, store: store})
		}
		return true
	})
	if len(targets) == 0 {
		return
	}
	sem := make(chan struct{}, spendRefreshConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		select {
		case <-p.ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(p.ctx, spendRefreshTimeout)
			defer cancel()
			startedAt := time.Now().UnixNano()
			if err := t.store.RefreshBudgetUsagesFromDatabase(ctx); err != nil {
				// Leave spendRefreshedAt unchanged so the next tick retries.
				p.logger.Warn("budget spend refresh failed for tenant %s: %v", t.tenantID, err)
				return
			}
			t.comp.spendRefreshedAt.Store(startedAt)
		}(t)
	}
	wg.Wait()
}
