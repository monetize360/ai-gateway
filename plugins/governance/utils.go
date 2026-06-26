// Package governance provides utility functions for the governance plugin
package governance

import (
	"context"
	"slices"
	"strings"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/valyala/fasthttp"
)

// VirtualKeyIDFromBifrostContext returns the virtual key UUID injected by tenant JWT middleware.
func VirtualKeyIDFromBifrostContext(ctx *schemas.BifrostContext) *string {
	if ctx == nil {
		return nil
	}
	if id := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyID); id != "" {
		return bifrost.Ptr(id)
	}
	if id := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyVirtualKey); id != "" {
		return bifrost.Ptr(id)
	}
	return nil
}

// VirtualKeyIDFromFastHTTPContext returns the virtual key UUID from tenant JWT user values.
func VirtualKeyIDFromFastHTTPContext(ctx *fasthttp.RequestCtx) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.UserValue(schemas.BifrostContextKeyGovernanceVirtualKeyID).(string); ok && id != "" {
		return id
	}
	if id, ok := ctx.UserValue(schemas.BifrostContextKeyVirtualKey).(string); ok && id != "" {
		return id
	}
	return ""
}

// stampVirtualKeyOrgContext sets the resolved governance org (usage scope) on context.
func stampVirtualKeyOrgContext(ctx *schemas.BifrostContext, vk *configstoreTables.TableVirtualKey) {
	if ctx == nil || vk == nil {
		return
	}
	if scopeOrgID := vk.GovernanceScopeOrgID(); scopeOrgID != nil {
		ctx.SetValue(schemas.BifrostContextKeyGovernanceOrgID, *scopeOrgID)
	}
}

// configModelIDResolver resolves config provider/model UUIDs from runtime names.
type configModelIDResolver interface {
	ResolveConfigProviderID(provider schemas.ModelProvider) string
	ResolveConfigModelID(provider schemas.ModelProvider, model string) string
}

// stampRoutingSourceModelIDsContext records config_providers/config_models IDs for the
// incoming request before routing. Target provider/model are stored on the log as provider/model.
func stampRoutingSourceModelIDsContext(ctx *schemas.BifrostContext, store configModelIDResolver, provider schemas.ModelProvider, model string) {
	if ctx == nil || store == nil || model == "" {
		return
	}
	if sourceProviderID := store.ResolveConfigProviderID(provider); sourceProviderID != "" {
		ctx.SetValue(schemas.BifrostContextKeyGovernanceRoutingSourceProviderID, sourceProviderID)
	}
	if sourceModelID := store.ResolveConfigModelID(provider, model); sourceModelID != "" {
		ctx.SetValue(schemas.BifrostContextKeyGovernanceRoutingSourceModelID, sourceModelID)
	}
}

// stampRoutingSourceModelIDsContextIfUnset stamps source IDs only when not already on context
// (e.g. PreLLMHook for SDK requests after HTTP transport may have already stamped).
func stampRoutingSourceModelIDsContextIfUnset(ctx *schemas.BifrostContext, store configModelIDResolver, provider schemas.ModelProvider, model string) {
	if ctx == nil {
		return
	}
	if bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceRoutingSourceModelID) != "" {
		return
	}
	stampRoutingSourceModelIDsContext(ctx, store, provider, model)
}

// getWeight safely dereferences a *float64 weight pointer, returning 1.0 as default if nil.
// This allows distinguishing between "not set" (nil -> 1.0) and "explicitly set to 0" (0.0).
func getWeight(w *float64) float64 {
	if w == nil {
		return 1.0
	}
	return *w
}

func blockedModelCandidates(model string) []string {
	_, normalized := schemas.ParseModelString(model, "")

	if strings.EqualFold(model, normalized) {
		return []string{model}
	}

	return []string{model, normalized}
}

func isModelBlockedByList(blacklist schemas.BlackList, model string) bool {
	if blacklist.IsBlockAll() {
		return true
	}

	modelForms := blockedModelCandidates(model)
	for _, blocked := range blacklist {
		blockedForms := blockedModelCandidates(blocked)
		for _, form := range modelForms {
			if slices.ContainsFunc(blockedForms, func(blockedForm string) bool {
				return strings.EqualFold(blockedForm, form)
			}) {
				return true
			}
		}
	}

	return false
}

// filterModelsForVirtualKey filters models based on virtual key and org provider configs.
func (p *GovernancePlugin) filterModelsForVirtualKey(
	ctx context.Context,
	models []schemas.Model,
	virtualKeyID string,
) []schemas.Model {
	comp := p.getComponentsForContext(ctx)
	if comp == nil {
		return []schemas.Model{}
	}
	vk, exists := comp.store.GetVirtualKey(ctx, virtualKeyID)
	if !exists {
		p.logger.Warn("[Governance] Virtual key not found for list models filtering: %s", virtualKeyID)
		return []schemas.Model{}
	}

	filteredModels := make([]schemas.Model, 0, len(models))
	for _, model := range models {
		provider, modelName := schemas.ParseModelString(model.ID, "")
		if comp.resolver.isModelAllowed(vk, provider, modelName) {
			filteredModels = append(filteredModels, model)
		}
	}

	return filteredModels
}
