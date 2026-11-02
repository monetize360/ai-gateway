package governance

import (
	"context"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

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
	return comp, nil
}

// refreshTenantFromRedis coalesces refreshes per tenant. A Redis snapshot
// replaces the already-warm in-memory governance data atomically.
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

	snapshot, err := p.redisCache.load(ctx, tenantID)
	if err == nil && snapshot != nil {
		localStore.ReplaceFromRedisSnapshot(ctx, snapshot)
		call.loaded = true
	}
	call.err = err
	close(call.done)
	p.tenantRefreshes.Delete(tenantID)
	return call.loaded, call.err
}
