package tenantstore

import (
	"context"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// TenantIDFromContext reads BifrostContextKeyTenantID from a BifrostContext,
// standard context, or fasthttp request context.
func TenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if reqCtx, ok := ctx.(*fasthttp.RequestCtx); ok {
		if tenantID, ok := reqCtx.UserValue(schemas.BifrostContextKeyTenantID).(string); ok && tenantID != "" {
			return tenantID
		}
	}
	if bc, ok := ctx.(*schemas.BifrostContext); ok && bc != nil {
		return bifrost.GetStringFromContext(bc, schemas.BifrostContextKeyTenantID)
	}
	tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	return tenantID
}
