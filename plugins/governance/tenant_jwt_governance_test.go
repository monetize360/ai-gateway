package governance

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tenant JWT auth attributes governance to the virtual key id only (no user id on context).
func TestEvaluateGovernanceRequest_TenantJWTEnforcesVKRateLimit(t *testing.T) {
	const tenantID = "978f2ee2-c0e7-4d62-aef0-a5d3a67e64ad"

	logger := NewMockLogger()
	rateLimit := buildRateLimitWithUsage("rl1", 0, 0, 1, 1)
	vk := buildVirtualKeyWithRateLimit("vk1", "vk1", "Test VK", rateLimit)
	vk.AllowedModelConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		buildProviderConfig("fakellm-openai", []string{"*"}),
	}

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		RateLimits:  []configstoreTables.TableRateLimit{*rateLimit},
	}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	plugin.tenantComponents.Store(tenantID, &tenantGovernanceComponents{
		store:    store,
		resolver: NewBudgetResolver(store, nil, logger, nil),
	})

	parentCtx := context.WithValue(context.Background(), schemas.BifrostContextKeyTenantID, tenantID)
	parentCtx = context.WithValue(parentCtx, schemas.BifrostContextKeyVirtualKey, "vk1")
	ctx := schemas.NewBifrostContext(parentCtx, schemas.NoDeadline)

	result, bifrostErr := plugin.EvaluateGovernanceRequest(ctx, &EvaluationRequest{
		VirtualKey: "vk1",
		Provider:   schemas.ModelProvider("fakellm-openai"),
		Model:      "gpt-4o-mini",
	}, schemas.ChatCompletionRequest)

	require.NotNil(t, result)
	require.Equal(t, DecisionRateLimited, result.Decision)
	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, 429, *bifrostErr.StatusCode)
	assert.Contains(t, bifrostErr.Error.Message, "rate limit")
}

func TestPostLLMHook_TenantJWTIncrementsVKRateLimitUsage(t *testing.T) {
	const tenantID = "978f2ee2-c0e7-4d62-aef0-a5d3a67e64ad"

	logger := NewMockLogger()
	rateLimit := buildRateLimitWithUsage("rl1", 0, 0, 1, 0)
	vk := buildVirtualKeyWithRateLimit("vk1", "vk1", "Test VK", rateLimit)
	vk.AllowedModelConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		buildProviderConfig("fakellm-openai", []string{"*"}),
	}

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		RateLimits:  []configstoreTables.TableRateLimit{*rateLimit},
	}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	plugin.tenantComponents.Store(tenantID, &tenantGovernanceComponents{
		store:    store,
		resolver: NewBudgetResolver(store, nil, logger, nil),
		tracker:  NewUsageTracker(context.Background(), store, NewBudgetResolver(store, nil, logger, nil), nil, logger),
	})

	parentCtx := context.WithValue(context.Background(), schemas.BifrostContextKeyTenantID, tenantID)
	parentCtx = context.WithValue(parentCtx, schemas.BifrostContextKeyVirtualKey, "vk1")
	parentCtx = context.WithValue(parentCtx, schemas.BifrostContextKeyRequestID, "req-1")
	ctx := schemas.NewBifrostContext(parentCtx, schemas.NoDeadline)

	result := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: "gpt-4o-mini",
			Usage: &schemas.BifrostLLMUsage{TotalTokens: 10},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               schemas.ModelProvider("fakellm-openai"),
				OriginalModelRequested: "gpt-4o-mini",
			},
		},
	}

	_, _, err = plugin.PostLLMHook(ctx, result, nil)
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)

	_, bifrostErr := plugin.EvaluateGovernanceRequest(ctx, &EvaluationRequest{
		VirtualKey: "vk1",
		Provider:   schemas.ModelProvider("fakellm-openai"),
		Model:      "gpt-4o-mini",
	}, schemas.ChatCompletionRequest)

	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, 429, *bifrostErr.StatusCode)
}

func TestEvaluateGovernanceRequest_EnterpriseUserAuthSkipsVKRateLimit(t *testing.T) {
	logger := NewMockLogger()
	rateLimit := buildRateLimitWithUsage("rl1", 0, 0, 1, 1)
	vk := buildVirtualKeyWithRateLimit("vk1", "vk1", "Test VK", rateLimit)
	vk.AllowedModelConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		buildProviderConfig("openai", []string{"*"}),
	}

	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
		RateLimits:  []configstoreTables.TableRateLimit{*rateLimit},
	}, nil)
	require.NoError(t, err)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)

	parentCtx := context.WithValue(context.Background(), schemas.BifrostContextKeyUserID, "enterprise-user")
	parentCtx = context.WithValue(parentCtx, schemas.BifrostContextKeyVirtualKey, "vk1")
	ctx := schemas.NewBifrostContext(parentCtx, schemas.NoDeadline)

	result, bifrostErr := plugin.EvaluateGovernanceRequest(ctx, &EvaluationRequest{
		VirtualKey: "vk1",
		Provider:   schemas.OpenAI,
		Model:      "gpt-4",
		UserID:     "enterprise-user",
	}, schemas.ChatCompletionRequest)

	require.NotNil(t, result)
	assert.Equal(t, DecisionAllow, result.Decision)
	assert.Nil(t, bifrostErr)
}
