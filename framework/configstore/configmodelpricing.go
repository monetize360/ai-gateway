package configstore

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// ConfigModelPricingIndex maps provider:model keys to config_models pricing rows.
type ConfigModelPricingIndex struct {
	byKey map[string]*tables.TableModel
}

// BuildConfigModelPricingIndex loads active config_models rows and indexes them by
// provider name and model name (same key format as governance budget cost lookup).
func BuildConfigModelPricingIndex(ctx context.Context, store ConfigStore) (*ConfigModelPricingIndex, error) {
	if store == nil {
		return &ConfigModelPricingIndex{byKey: make(map[string]*tables.TableModel)}, nil
	}

	configModels, err := store.GetConfigModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load config models: %w", err)
	}

	providers, err := store.GetProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load providers: %w", err)
	}

	providerNameByID := make(map[string]string, len(providers))
	for i := range providers {
		providerNameByID[providers[i].ID] = providers[i].Name
	}

	byKey := make(map[string]*tables.TableModel, len(configModels))
	for i := range configModels {
		cm := &configModels[i]
		providerName, ok := providerNameByID[cm.ProviderID]
		if !ok || providerName == "" {
			continue
		}
		byKey[configModelPricingKey(providerName, cm.Name)] = cm
	}

	return &ConfigModelPricingIndex{byKey: byKey}, nil
}

func configModelPricingKey(provider, model string) string {
	return fmt.Sprintf("%s:%s", provider, model)
}

// CalculateTokenCost returns dollar cost from config_models per-token rates.
// It tries resolvedModel then aliasModel when rates are missing for the resolved name.
func (idx *ConfigModelPricingIndex) CalculateTokenCost(provider, resolvedModel, aliasModel string, promptTokens, completionTokens int) float64 {
	if idx == nil || promptTokens == 0 && completionTokens == 0 {
		return 0
	}
	for _, model := range []string{resolvedModel, aliasModel} {
		if model == "" {
			continue
		}
		if cost := idx.calculateForModel(provider, model, promptTokens, completionTokens); cost > 0 {
			return cost
		}
	}
	return 0
}

// LookupCurrencyID returns the config_models.currency UUID for the given provider+model pair.
// It tries resolvedModel then aliasModel, returning empty string when not found.
func (idx *ConfigModelPricingIndex) LookupCurrencyID(provider, resolvedModel, aliasModel string) string {
	if idx == nil {
		return ""
	}
	for _, model := range []string{resolvedModel, aliasModel} {
		if model == "" {
			continue
		}
		if cm, ok := idx.byKey[configModelPricingKey(provider, model)]; ok && cm != nil && cm.CurrencyID != nil && *cm.CurrencyID != "" {
			return *cm.CurrencyID
		}
	}
	return ""
}

func (idx *ConfigModelPricingIndex) calculateForModel(provider, model string, promptTokens, completionTokens int) float64 {
	if idx == nil {
		return 0
	}
	cm, ok := idx.byKey[configModelPricingKey(provider, model)]
	if !ok || cm == nil {
		return 0
	}
	var cost float64
	if cm.InputCostPerToken != nil {
		cost += float64(promptTokens) * *cm.InputCostPerToken
	}
	if cm.OutputCostPerToken != nil {
		cost += float64(completionTokens) * *cm.OutputCostPerToken
	}
	return cost
}

// TokenCountsFromResponse extracts prompt and completion token counts for config_models billing.
func TokenCountsFromResponse(result *schemas.BifrostResponse) (promptTokens, completionTokens int) {
	if result == nil {
		return 0, 0
	}
	switch {
	case result.TextCompletionResponse != nil && result.TextCompletionResponse.Usage != nil:
		return result.TextCompletionResponse.Usage.PromptTokens, result.TextCompletionResponse.Usage.CompletionTokens
	case result.ChatResponse != nil && result.ChatResponse.Usage != nil:
		return result.ChatResponse.Usage.PromptTokens, result.ChatResponse.Usage.CompletionTokens
	case result.ResponsesResponse != nil && result.ResponsesResponse.Usage != nil:
		return result.ResponsesResponse.Usage.InputTokens, result.ResponsesResponse.Usage.OutputTokens
	case result.ResponsesStreamResponse != nil && result.ResponsesStreamResponse.Response != nil && result.ResponsesStreamResponse.Response.Usage != nil:
		return result.ResponsesStreamResponse.Response.Usage.InputTokens, result.ResponsesStreamResponse.Response.Usage.OutputTokens
	case result.EmbeddingResponse != nil && result.EmbeddingResponse.Usage != nil:
		return result.EmbeddingResponse.Usage.PromptTokens, 0
	default:
		return 0, 0
	}
}
