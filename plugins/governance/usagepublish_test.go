package governance

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestBoundContext panics when Value is called from any goroutine other than
// the one that created it. That is the fasthttp RequestCtx failure mode: the
// handler's goroutine may read it, and a background goroutine must not.
type requestBoundContext struct {
	context.Context
	owner  uint64
	values map[any]any
}

func (c *requestBoundContext) Value(key any) any {
	if currentGoroutineID() != c.owner {
		panic("use of request context outside the request goroutine")
	}
	if value, ok := c.values[key]; ok {
		return value
	}
	if c.Context != nil {
		return c.Context.Value(key)
	}
	return nil
}

func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

type capturedUsagePublish struct {
	tenantID    string
	key         string
	message     map[string]any
	hasDeadline bool
}

type captureUsagePublisher struct {
	mu   sync.Mutex
	call *capturedUsagePublish
}

func (p *captureUsagePublisher) PublishUsage(ctx context.Context, tenantID, key string, message map[string]any) error {
	_, hasDeadline := ctx.Deadline()
	p.mu.Lock()
	p.call = &capturedUsagePublish{
		tenantID:    tenantID,
		key:         key,
		message:     message,
		hasDeadline: hasDeadline,
	}
	p.mu.Unlock()
	return nil
}

func (p *captureUsagePublisher) PublishAlert(context.Context, string, string, map[string]any) error {
	return nil
}

func (p *captureUsagePublisher) get() *capturedUsagePublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call
}

func TestPostLLMHook_PublishesInferenceUsageAfterRequestContextReleased(t *testing.T) {
	const (
		tenantID = "tenant-1"
		orgID    = "org-1"
		vkUserID = "vk-user"
		orgUnit  = "ou-1"
	)

	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{}, nil)
	require.NoError(t, err)

	providerName := "fakellm"
	modelName := "gemma-3-1b-it"
	serviceID := "svc-1"
	externalID := "ext-acct-1"
	store.providers.Store(providerName, &configstoreTables.TableProvider{ID: "prov-1", Name: providerName})
	store.applyGovernanceRefreshDelta(context.Background(), &configstore.GovernanceRefreshDelta{
		Accounts: []configstoreTables.TableAccount{{
			ID:                     "acct-1",
			ExternalID:             &externalID,
			CustomerOrganizationID: ptr(orgID),
		}},
		UserOrgUnits: []configstoreTables.TableUserOrgUnit{{
			ID:        vkUserID,
			OrgUnitID: ptr(orgUnit),
		}},
		ConfigModels: []configstoreTables.TableModel{{
			ID:           "model-1",
			ProviderID:   "prov-1",
			Name:         modelName,
			ServiceID:    &serviceID,
			ProviderName: providerName,
		}},
	})

	vk := buildVirtualKey("vk1", "vk1", "Test VK", true)
	vk.UserID = ptr(vkUserID)
	store.storeVirtualKey(vk)

	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, nil, nil, nil)
	require.NoError(t, err)
	plugin.tenantComponents.Store(tenantID, &tenantGovernanceComponents{
		store:   store,
		tracker: NewUsageTracker(context.Background(), store, nil, nil, logger),
	})
	publisher := &captureUsagePublisher{}
	plugin.SetUsageEventPublisher(publisher)
	t.Cleanup(func() {
		plugin.wg.Wait()
	})

	parent := &requestBoundContext{
		Context: context.Background(),
		owner:   currentGoroutineID(),
		values: map[any]any{
			schemas.BifrostContextKeyTenantID:               tenantID,
			schemas.BifrostContextKeyVirtualKey:             "vk1",
			schemas.BifrostContextKeyRequestID:              "req-1",
			schemas.BifrostContextKeyGovernanceOrgID:        orgID,
			schemas.BifrostContextKeyGovernanceVirtualKeyID: "vk-row",
			schemas.BifrostContextKeyBillingUserID:          "bill-user",
		},
	}
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	result := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: modelName,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     3,
				CompletionTokens: 7,
				TotalTokens:      10,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               schemas.ModelProvider(providerName),
				OriginalModelRequested: modelName,
				Latency:                60,
			},
		},
	}

	_, _, err = plugin.PostLLMHook(ctx, result, nil)
	require.NoError(t, err)
	// The request goroutine is done. Release the request-scoped values the way
	// fasthttp reuses RequestCtx. The Kafka payload must already have been copied.
	parent.values[schemas.BifrostContextKeyTenantID] = "released-tenant"
	parent.values[schemas.BifrostContextKeyRequestID] = "released-request"
	parent.values[schemas.BifrostContextKeyGovernanceOrgID] = "released-org"
	plugin.wg.Wait()

	call := publisher.get()
	require.NotNil(t, call, "InferenceUsage was not published")
	assert.True(t, call.hasDeadline, "publish must use a detached timeout, not the request context")
	assert.Equal(t, tenantID, call.tenantID)
	assert.Equal(t, "req-1", call.key)
	assert.Equal(t, "req-1", call.message["externalTransactionId"])
	assert.Equal(t, orgID, call.message["organizationId"])
	assert.Equal(t, serviceID, call.message["serviceId"])
	assert.Equal(t, externalID, call.message["billingAccountRef"])
	assert.Equal(t, orgUnit, call.message["orgUnitId"])
	assert.Equal(t, "vk1", call.message["virtualKeyId"])

	dimensions, ok := call.message["dimensions"].(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "3", dimensions["input_token"])
	assert.Equal(t, "7", dimensions["output_token"])
	assert.Equal(t, modelName, dimensions["model"])
	assert.Equal(t, providerName, dimensions["provider"])
	assert.Equal(t, "vk-row", dimensions["virtual_key_id"])
	assert.Equal(t, "60", dimensions["api_latency_ms"])
	assert.Equal(t, "60", dimensions["ttft_ms"])
}

func TestPostMCPHook_DoesNotReadRequestContext(t *testing.T) {
	const tenantID = "tenant-1"

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
	t.Cleanup(func() {
		plugin.wg.Wait()
	})

	parent := &requestBoundContext{
		Context: context.Background(),
		owner:   currentGoroutineID(),
		values: map[any]any{
			schemas.BifrostContextKeyTenantID:   tenantID,
			schemas.BifrostContextKeyVirtualKey: "vk1",
			schemas.BifrostContextKeyRequestID:  "req-mcp",
		},
	}
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	resp := &schemas.BifrostMCPResponse{}
	resp.ExtraFields.MCPRequestType = schemas.MCPRequestTypeExecuteTool
	resp.ExtraFields.ToolName = "echo"

	_, _, err = plugin.PostMCPHook(ctx, resp, nil)
	require.NoError(t, err)
	plugin.wg.Wait()

	_, bifrostErr := plugin.EvaluateGovernanceRequest(ctx, &EvaluationRequest{
		VirtualKey: "vk1",
		Provider:   schemas.ModelProvider("fakellm-openai"),
		Model:      "gpt-4o-mini",
	}, schemas.ChatCompletionRequest)
	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, 429, *bifrostErr.StatusCode)
}

func ptr[T any](value T) *T {
	return &value
}
