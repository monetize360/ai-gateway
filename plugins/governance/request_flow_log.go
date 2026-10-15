package governance

import (
	"fmt"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// vkNameFromCtx returns the Virtual Key name stamped on context, or "(none)".
func vkNameFromCtx(ctx *schemas.BifrostContext) string {
	if ctx == nil {
		return "(none)"
	}
	name := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyName)
	if name == "" {
		return "(none)"
	}
	return name
}

// logRequestFlow writes a concise [request-flow] Info line with vk= prefix.
func logRequestFlow(ctx *schemas.BifrostContext, logger schemas.Logger, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Info(fmt.Sprintf("[request-flow] vk=%q ", vkNameFromCtx(ctx))+format, args...)
}
