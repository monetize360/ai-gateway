package lib

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
	"github.com/valyala/fasthttp"
)

// Registry returns the tenant config registry.
func (c *Config) Registry() tenantstore.Resolver {
	if c == nil || c.TenantStore == nil {
		return nil
	}
	return c.TenantStore.Registry
}

// StoreFromRequestCtx resolves the tenant ConfigStore from the request context.
func (c *Config) StoreFromRequestCtx(ctx *fasthttp.RequestCtx) configstore.ConfigStore {
	if c == nil || c.TenantStore == nil || c.TenantStore.Registry == nil {
		return nil
	}
	return c.TenantStore.Registry.GetStoreFromRequestCtx(ctx)
}

// RequireStoreFromRequestCtx resolves the tenant ConfigStore or returns an error.
func (c *Config) RequireStoreFromRequestCtx(ctx *fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	if c == nil || c.TenantStore == nil || c.TenantStore.Registry == nil {
		return nil, fmt.Errorf("tenant store is not configured")
	}
	return c.TenantStore.Registry.RequireStoreFromRequestCtx(ctx)
}

// StoreFromContext resolves the tenant ConfigStore from a standard context.
func (c *Config) StoreFromContext(ctx context.Context) configstore.ConfigStore {
	if c == nil || c.TenantStore == nil || c.TenantStore.Registry == nil {
		return nil
	}
	return c.TenantStore.Registry.GetStoreFromContext(ctx)
}

// LogStoreFromContext resolves the tenant LogStore from ctx, falling back to the global store.
func (c *Config) LogStoreFromContext(ctx context.Context) logstore.LogStore {
	if c == nil {
		return nil
	}
	if c.TenantStore != nil && c.TenantStore.LogStoreManager != nil {
		if store := c.TenantStore.LogStoreManager.GetLogStoreFromContext(ctx); store != nil {
			return store
		}
	}
	return c.LogsStore
}

// LogStoreFromRequestCtx resolves the tenant LogStore from a fasthttp request context.
func (c *Config) LogStoreFromRequestCtx(ctx *fasthttp.RequestCtx) logstore.LogStore {
	if c == nil {
		return nil
	}
	if c.TenantStore != nil && c.TenantStore.LogStoreManager != nil {
		if tenantID := TenantIDFromRequest(ctx); tenantID != "" {
			return c.TenantStore.LogStoreManager.GetLogStoreForTenant(tenantID)
		}
	}
	return c.LogsStore
}

// LogStoreResolver returns the per-tenant log store manager when configured.
func (c *Config) LogStoreResolver() tenantstore.LogStoreResolver {
	if c == nil || c.TenantStore == nil {
		return nil
	}
	return c.TenantStore.LogStoreManager
}

// TenantIDFromContext reads a tenant ID from a standard or fasthttp request context.
func TenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if reqCtx, ok := ctx.(*fasthttp.RequestCtx); ok {
		return TenantIDFromRequest(reqCtx)
	}
	tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	return tenantID
}

// TenantIDFromRequest reads the tenant ID injected by TenantMiddleware.
func TenantIDFromRequest(ctx *fasthttp.RequestCtx) string {
	if ctx == nil {
		return ""
	}
	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	return tenantID
}

// ContextWithTimeoutPreservingTenant returns a detached context.Context with the
// same tenant ID as parent and the given timeout. Use this for background work
// started from a tenant-scoped HTTP request so StoreFromContext keeps resolving
// the correct tenant database.
func ContextWithTimeoutPreservingTenant(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	tenantID := TenantIDFromContext(parent)
	if tenantID == "" {
		return context.WithTimeout(parent, timeout)
	}
	base := context.WithValue(context.Background(), schemas.BifrostContextKeyTenantID, tenantID)
	return context.WithTimeout(base, timeout)
}
