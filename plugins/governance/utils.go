// Package governance provides utility functions for the governance plugin
package governance

import (
	"context"
	"slices"
	"strings"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
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

// filterModelsForVirtualKey filters models based on virtual key's provider configs
// Returns only models that are allowed by the virtual key's ProviderConfigs
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
		return []schemas.Model{} // VK not found, return empty list
	}

	// Empty ProviderConfigs means no provider-level restrictions (allow all models).
	if len(vk.ProviderConfigs) == 0 {
		return models
	}

	// Filter models based on ProviderConfigs
	filteredModels := make([]schemas.Model, 0, len(models))
	for _, model := range models {
		provider, modelName := schemas.ParseModelString(model.ID, "")

		// Pre-pass: if any matching config blacklists the model, block it entirely.
		isBlocked := false
		for _, pc := range vk.ProviderConfigs {
			if pc.Provider == string(provider) && isModelBlockedByList(pc.BlacklistedModels, modelName) {
				isBlocked = true
				break
			}
		}
		if isBlocked {
			continue
		}

		// Allowlist check — model is allowed if any matching config permits it.
		isAllowed := false
		for _, pc := range vk.ProviderConfigs {
			if pc.Provider == string(provider) {
				if p.modelCatalog != nil && p.inMemoryStore != nil {
					providerConfig, ok := p.inMemoryStore.GetConfiguredProviders()[provider]
					providerConfigPtr := &providerConfig
					if !ok {
						providerConfigPtr = nil
					}
					if p.modelCatalog.IsModelAllowedForProvider(provider, modelName, providerConfigPtr, pc.AllowedModels) {
						isAllowed = true
						break
					}
				} else {
					if pc.AllowedModels.IsAllowed(modelName) {
						isAllowed = true
						break
					}
				}
			}
		}

		if isAllowed {
			filteredModels = append(filteredModels, model)
		}
	}

	return filteredModels
}
