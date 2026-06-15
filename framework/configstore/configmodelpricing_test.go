package configstore

import (
	"context"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

func TestConfigModelPricingIndex_CalculateTokenCost(t *testing.T) {
	inputRate := 0.000001
	outputRate := 0.000002
	idx := &ConfigModelPricingIndex{
		byKey: map[string]*tables.TableModel{
			configModelPricingKey("openai", "gpt-4o"): {
				Name:               "gpt-4o",
				InputCostPerToken:  bifrost.Ptr(inputRate),
				OutputCostPerToken: bifrost.Ptr(outputRate),
			},
		},
	}

	cost := idx.CalculateTokenCost("openai", "gpt-4o", "", 1000, 500)
	want := 1000*inputRate + 500*outputRate
	if cost != want {
		t.Fatalf("expected cost %.8f, got %.8f", want, cost)
	}
}

func TestConfigModelPricingIndex_FallsBackToAlias(t *testing.T) {
	inputRate := 0.000001
	idx := &ConfigModelPricingIndex{
		byKey: map[string]*tables.TableModel{
			configModelPricingKey("openai", "gpt-4o"): {
				Name:              "gpt-4o",
				InputCostPerToken: bifrost.Ptr(inputRate),
			},
		},
	}

	cost := idx.CalculateTokenCost("openai", "gpt-4o-latest", "gpt-4o", 1000, 0)
	if cost != 1000*inputRate {
		t.Fatalf("expected alias fallback cost %.8f, got %.8f", 1000*inputRate, cost)
	}
}

func TestBuildConfigModelPricingIndex(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableProvider{}, &tables.TableModel{}))

	ctx := context.Background()
	providerID := "provider-openai"
	require.NoError(t, store.DB().Create(&tables.TableProvider{
		ID:   providerID,
		Name: "openai",
	}).Error)
	require.NoError(t, store.DB().Create(&tables.TableModel{
		ID:                 "model-gpt-4o-mini",
		ProviderID:         providerID,
		Name:               "gpt-4o-mini",
		InputCostPerToken:  bifrost.Ptr(0.0000001),
		OutputCostPerToken: bifrost.Ptr(0.0000004),
	}).Error)

	idx, err := BuildConfigModelPricingIndex(ctx, store)
	require.NoError(t, err)

	cost := idx.CalculateTokenCost("openai", "gpt-4o-mini", "", 10_000, 2_000)
	if cost <= 0 {
		t.Fatalf("expected positive cost, got %v", cost)
	}
}

func TestTokenCountsFromResponse_Chat(t *testing.T) {
	prompt, completion := TokenCountsFromResponse(&schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5},
		},
	})
	if prompt != 10 || completion != 5 {
		t.Fatalf("unexpected token counts: prompt=%d completion=%d", prompt, completion)
	}
}
