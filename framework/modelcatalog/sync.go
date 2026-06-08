package modelcatalog

import (
	"encoding/json"
	"slices"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// populateModelParamsFromPricing extracts max_output_tokens from pricing entries
// and populates the model params cache so that providers can look up max output
// tokens without a separate model-parameters sync.
func (mc *ModelCatalog) populateModelParamsFromPricing(pricingData map[string]PricingEntry) {
	modelParamsEntries := make(map[string]providerUtils.ModelParams)
	for modelKey, entry := range pricingData {
		if entry.MaxOutputTokens != nil {
			modelParamsEntries[modelKey] = providerUtils.ModelParams{
				MaxOutputTokens: entry.MaxOutputTokens,
			}
		}
	}
	if len(modelParamsEntries) > 0 {
		providerUtils.BulkSetModelParams(modelParamsEntries)
	}
}

func (mc *ModelCatalog) applyModelParameters(paramsData map[string]json.RawMessage) {
	modelParamsEntries := make(map[string]providerUtils.ModelParams, len(paramsData))
	newResponseTypes := make(map[string][]string, len(paramsData))
	newParamsIndex := make(map[string][]string, len(paramsData))

	for model, rawData := range paramsData {
		var parsed modelParametersParseResult
		if err := json.Unmarshal(rawData, &parsed); err != nil {
			mc.logger.Warn("model-parameters: skipping malformed parameters for model %s: %v", model, err)
			continue
		}

		outputs := make([]string, 0, len(parsed.SupportedEndpoints))
		for _, endpoint := range parsed.SupportedEndpoints {
			if normalized := normalizeEndpointToOutputType(endpoint); normalized != "" && !slices.Contains(outputs, normalized) {
				outputs = append(outputs, normalized)
			}
		}

		if parsed.Mode != nil {
			if normalized := normalizeModeToOutputType(*parsed.Mode); normalized != "" && !slices.Contains(outputs, normalized) {
				outputs = append(outputs, normalized)
			}
		}

		if !slices.Contains(outputs, "text_completion") {
			provider := gjson.GetBytes(rawData, "provider")
			if provider.Exists() {
				key := makeKey(model, normalizeProvider(provider.String()), normalizeRequestType(schemas.TextCompletionRequest))

				mc.mu.RLock()
				_, ok := mc.pricingData[key]
				mc.mu.RUnlock()
				if ok {
					outputs = append(outputs, "text_completion")
				}
			}
		}

		if len(outputs) > 0 {
			newResponseTypes[model] = outputs
		}

		supported := extractSupportedParams(&parsed)
		if len(supported) > 0 {
			newParamsIndex[model] = supported
		}

		var p struct {
			MaxOutputTokens *int `json:"max_output_tokens"`
		}
		if err := json.Unmarshal(rawData, &p); err == nil && (p.MaxOutputTokens != nil || parsed.VertexMultiRegionOnly != nil) {
			modelParamsEntries[model] = providerUtils.ModelParams{
				MaxOutputTokens:         p.MaxOutputTokens,
				IsVertexMultiRegionOnly: parsed.VertexMultiRegionOnly,
			}
		}
	}

	mc.mu.Lock()
	mc.supportedResponseTypes = newResponseTypes
	mc.supportedParams = newParamsIndex
	mc.mu.Unlock()

	if len(modelParamsEntries) > 0 {
		providerUtils.BulkSetModelParams(modelParamsEntries)
	}
}
