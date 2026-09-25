// Package governance provides utility functions for the governance plugin
package governance

import (
	"context"
	"net/url"
	"slices"
	"strings"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/providers/gemini"
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

// routedModel is the model a request currently carries, along with where it was read
// from so it can be written back to the same place.
type routedModel struct {
	Model         string
	IsGeminiPath  bool
	IsBedrockPath bool
	GenAISuffix   string
}

// resolveRoutedModel reads the model a request should be routed on, preferring a value
// an earlier routing stage placed on the context over the original URL path param. Any
// genai request suffix is stripped off and returned separately.
// Returns false when the request carries no model to route.
func resolveRoutedModel(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, body map[string]any) (routedModel, bool) {
	resolved := routedModel{
		IsGeminiPath:  strings.Contains(req.Path, "/genai"),
		IsBedrockPath: strings.Contains(req.Path, "/bedrock"),
	}

	modelValue, hasModel := body["model"]
	if !hasModel {
		switch {
		case resolved.IsGeminiPath:
			// genai carries the model in the URL path; a routing rule may have already
			// rewritten it onto the context as "provider/model:suffix".
			if ctxModel, ok := ctx.Value("model").(string); ok && ctxModel != "" {
				modelValue = ctxModel
			} else {
				modelValue = req.CaseInsensitivePathParamLookup("model")
			}
		case resolved.IsBedrockPath:
			if ctxModelID, ok := ctx.Value("modelId").(string); ok && ctxModelID != "" {
				modelValue = ctxModelID
			} else {
				rawModelID := req.CaseInsensitivePathParamLookup("modelId")
				if rawModelID == "" {
					return resolved, false
				}
				// Bedrock model IDs may be URL-encoded, e.g. anthropic%2Fclaude-3-5-sonnet.
				decoded, err := url.PathUnescape(rawModelID)
				if err != nil {
					decoded = rawModelID
				}
				modelValue = decoded
			}
		default:
			return resolved, false
		}
	}

	modelStr, ok := modelValue.(string)
	if !ok || modelStr == "" {
		return resolved, false
	}

	if resolved.IsGeminiPath {
		for _, sfx := range gemini.GeminiRequestSuffixPaths {
			if before, found := strings.CutSuffix(modelStr, sfx); found {
				modelStr = before
				resolved.GenAISuffix = sfx
				break
			}
		}
	}

	resolved.Model = modelStr
	return resolved, true
}

// writeModelBack records the routed provider/model where the integration will read
// it back from: genai and bedrock carry the model in the URL path and their
// pre-callbacks read it off the context, everything else reads the JSON body.
//
// provider may be empty, in which case the model is written unprefixed so a later
// routing stage can still supply the prefix. genaiRequestSuffix is only appended on
// the genai path and should be the suffix that was stripped off the incoming model.
func writeModelBack(ctx *schemas.BifrostContext, body map[string]any, isGeminiPath, isBedrockPath bool, provider schemas.ModelProvider, model, genaiRequestSuffix string) {
	newModel := model
	if isGeminiPath {
		newModel += genaiRequestSuffix
	}
	if provider != "" {
		newModel = string(provider) + "/" + newModel
	}

	switch {
	case isGeminiPath:
		ctx.SetValue("model", newModel)
	case isBedrockPath:
		ctx.SetValue("modelId", newModel)
	default:
		body["model"] = newModel
	}
}

// setFallbacksIfAbsent records a fallback chain on the payload only when no earlier
// routing stage set one. Earlier stages win, so a chain from a matched routing rule
// is never replaced by semantic routing or by load balancing.
// Returns whether the chain was written.
func setFallbacksIfAbsent(body map[string]any, fallbacks []string) bool {
	if len(fallbacks) == 0 {
		return false
	}
	if _, exists := body["fallbacks"]; exists {
		return false
	}
	body["fallbacks"] = fallbacks
	return true
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
