package handlers

import (
	"encoding/json"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/guardrails"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// GuardrailHandler serves the guardrail catalog, templates, and test call.
type GuardrailHandler struct {
	config *lib.Config
}

// NewGuardrailHandler creates the guardrail admin handler.
func NewGuardrailHandler(config *lib.Config) *GuardrailHandler {
	return &GuardrailHandler{config: config}
}

// RegisterRoutes registers guardrail catalog routes.
func (h *GuardrailHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/guardrails/types", lib.ChainMiddlewares(h.getTypes, middlewares...))
	r.GET("/api/guardrails/templates", lib.ChainMiddlewares(h.getTemplates, middlewares...))
	r.POST("/api/guardrails/test", lib.ChainMiddlewares(h.test, middlewares...))
}

func (h *GuardrailHandler) plugin(ctx *fasthttp.RequestCtx) (*guardrails.Plugin, bool) {
	plugin, err := lib.FindPluginAs[*guardrails.Plugin](h.config, guardrails.PluginName)
	if err != nil || plugin == nil {
		SendError(ctx, fasthttp.StatusNotFound, "guardrails plugin is not enabled")
		return nil, false
	}
	return plugin, true
}

func (h *GuardrailHandler) getTypes(ctx *fasthttp.RequestCtx) {
	plugin, ok := h.plugin(ctx)
	if !ok {
		return
	}
	SendJSON(ctx, map[string]any{"types": plugin.Types()})
}

func (h *GuardrailHandler) getTemplates(ctx *fasthttp.RequestCtx) {
	plugin, ok := h.plugin(ctx)
	if !ok {
		return
	}
	SendJSON(ctx, map[string]any{"templates": plugin.PolicyTemplates()})
}

func (h *GuardrailHandler) test(ctx *fasthttp.RequestCtx) {
	plugin, ok := h.plugin(ctx)
	if !ok {
		return
	}
	tenantID, _ := ctx.UserValue(schemas.BifrostContextKeyTenantID).(string)
	var request guardrails.TestRequest
	if err := json.Unmarshal(ctx.PostBody(), &request); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid guardrail test request")
		return
	}
	result, err := plugin.Test(ctx, tenantID, request)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSON(ctx, result)
}
