package lib

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
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
