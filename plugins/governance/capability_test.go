package governance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testVisionModel = "test-vision-model"
	testTextModel   = "test-text-model"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// newCapabilityTestPlugin builds a plugin whose catalog knows exactly two models: one
// that accepts images and a cheaper one that does not.
func newCapabilityTestPlugin() *GovernancePlugin {
	logger := NewMockLogger()
	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.SeedForTest(logger, []modelcatalog.TestModel{
		{
			Provider:           schemas.OpenAI,
			Model:              testVisionModel,
			ResponseTypes:      []string{string(schemas.ChatCompletionRequest)},
			SupportedParams:    []string{modelcatalog.CapabilityVision, "tools"},
			MaxInputTokens:     intPtr(128000),
			MaxOutputTokens:    intPtr(16384),
			InputCostPerToken:  floatPtr(0.000005),
			OutputCostPerToken: floatPtr(0.000015),
			SupportsVision:     boolPtr(true),
		},
		{
			Provider:           schemas.OpenAI,
			Model:              testTextModel,
			ResponseTypes:      []string{string(schemas.ChatCompletionRequest)},
			SupportedParams:    []string{"tools"},
			MaxInputTokens:     intPtr(128000),
			MaxOutputTokens:    intPtr(16384),
			InputCostPerToken:  floatPtr(0.0000001),
			OutputCostPerToken: floatPtr(0.0000004),
		},
	})
	return &GovernancePlugin{modelCatalog: catalog, logger: logger}
}

func testCandidates(models ...string) []routeCandidate {
	config := configstoreTables.TableAllowedModelConfig{
		Provider:      string(schemas.OpenAI),
		AllowedModels: schemas.WhiteList(models),
	}
	candidates := make([]routeCandidate, 0, len(models))
	for _, model := range models {
		candidates = append(candidates, routeCandidate{Provider: schemas.OpenAI, Model: model, Config: config})
	}
	return candidates
}

// eligibleModels runs the hard capability filter and returns the survivors' names.
func eligibleModels(p *GovernancePlugin, candidates []routeCandidate, profile *RequestProfile) []string {
	survivors := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if ok, _ := p.candidateSatisfiesProfile(candidate, profile, schemas.ChatCompletionRequest); ok {
			survivors = append(survivors, candidate.Model)
		}
	}
	return survivors
}

// imagePayload is an OpenAI chat request carrying one image part.
func imagePayload() map[string]any {
	return map[string]any{
		"model": "openai/" + testTextModel,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is in this picture?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}},
				},
			},
		},
	}
}

func TestRequestProfileDetectsImagePart(t *testing.T) {
	profile := buildRequestProfile(imagePayload())

	require.True(t, profile.Recognized, "an OpenAI chat payload must be recognized")
	assert.True(t, profile.HasImage, "an image_url content part must set HasImage")
	assert.False(t, profile.HasAudio)
	assert.False(t, profile.HasPDF)
	assert.Equal(t, 1, profile.MessageCount)
	assert.Positive(t, profile.EstInputTokens, "the text part must contribute to the token estimate")
	logged := profile.logFields()
	assert.Contains(t, logged, "recognized=true")
	assert.Contains(t, logged, "image=true")
	assert.Contains(t, logged, "pdf=false")
	assert.Contains(t, logged, "audio=false")
	assert.Contains(t, logged, "tools=false")
	assert.Contains(t, logged, "messages=1")
}

// An image request must land on the vision-capable model even though the text-only model
// is two orders of magnitude cheaper — the capability filter is hard, not a preference.
func TestSemanticRoutingSelectsVisionModelForImageRequest(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(imagePayload())

	eligible := eligibleModels(p, testCandidates(testTextModel, testVisionModel), profile)
	require.Equal(t, []string{testVisionModel}, eligible, "only the vision model may survive an image request")

	ranked := p.rankCandidates(testCandidates(testVisionModel), profile, &SemanticRoutingConfig{Enabled: true}, nil)
	require.NotEmpty(t, ranked)
	assert.Equal(t, testVisionModel, ranked[0].Model)
}

// With no capable candidate the stage must decline so load balancing runs unchanged.
func TestSemanticRoutingDeclinesWhenOnlyTextModelAvailable(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(imagePayload())

	assert.Empty(t, eligibleModels(p, testCandidates(testTextModel), profile),
		"a text-only pool must yield no eligible candidate for an image request")

	// A text-only request over the same pool must still be routable, otherwise the
	// filter is simply rejecting everything.
	textProfile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	assert.Equal(t, []string{testTextModel}, eligibleModels(p, testCandidates(testTextModel), textProfile))
}

// A model the catalog does not know cannot be gated safely, so it is never eligible.
func TestSemanticRoutingRejectsUnknownModel(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})

	assert.Empty(t, eligibleModels(p, testCandidates("model-not-in-catalog"), profile))
}

// Cost decides between two candidates that both satisfy the request.
func TestSemanticRoutingPrefersCheaperCapableModel(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})

	ranked := p.rankCandidates(testCandidates(testVisionModel, testTextModel), profile, &SemanticRoutingConfig{Enabled: true}, nil)
	require.Len(t, ranked, 2)
	assert.Equal(t, testTextModel, ranked[0].Model, "the cheaper capable model must win on cost")
}

// An explicit class preference outranks cost.
func TestSemanticRoutingPreferenceBeatsCost(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	cfg := &SemanticRoutingConfig{
		Enabled:          true,
		DefaultClass:     "reasoning",
		Classes:          map[string][]string{"reasoning": {testVisionModel}},
		PreferenceWeight: floatPtr(2.0),
		CostWeight:       floatPtr(0.1),
		TaskWeight:       floatPtr(0),
		TierWeight:       floatPtr(0),
	}

	ranked := p.rankCandidates(testCandidates(testVisionModel, testTextModel), profile, cfg, nil)
	require.Len(t, ranked, 2)
	assert.Equal(t, testVisionModel, ranked[0].Model, "the first preference must win despite being pricier")
}

// A rule-level fallbacks list overrides the class preference list.
func TestSemanticRoutingRuleFallbacksOverrideClassPreference(t *testing.T) {
	p := newCapabilityTestPlugin()
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	cfg := &SemanticRoutingConfig{
		Enabled:          true,
		DefaultClass:     "reasoning",
		Classes:          map[string][]string{"reasoning": {testVisionModel}},
		PreferenceWeight: floatPtr(2.0),
		CostWeight:       floatPtr(0.1),
		TaskWeight:       floatPtr(0),
		TierWeight:       floatPtr(0),
	}

	ranked := p.rankCandidates(testCandidates(testVisionModel, testTextModel), profile, cfg, []string{"openai/" + testTextModel})
	require.Len(t, ranked, 2)
	assert.Equal(t, testTextModel, ranked[0].Model, "the rule preference list must beat the class list")
}

// The context filter must reject a request that cannot fit, using the estimate plus margin.
func TestSemanticRoutingRejectsOversizedRequest(t *testing.T) {
	p := newCapabilityTestPlugin()
	huge := make([]byte, 700_000) // ~175k estimated tokens against a 128k window
	for i := range huge {
		huge[i] = 'a'
	}
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": string(huge)}},
	})

	assert.Empty(t, eligibleModels(p, testCandidates(testTextModel), profile))
}

// writeModelBack must target the place each integration reads the model back from.
func TestWriteModelBackTargets(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	defer ctx.Cancel()

	body := map[string]any{"model": "gpt-4o"}
	writeModelBack(ctx, body, false, false, schemas.OpenAI, "gpt-4o-mini", "")
	assert.Equal(t, "openai/gpt-4o-mini", body["model"])

	writeModelBack(ctx, body, true, false, schemas.Gemini, "gemini-2.0-flash", ":streamGenerateContent")
	assert.Equal(t, "gemini/gemini-2.0-flash:streamGenerateContent", ctx.Value("model"))

	writeModelBack(ctx, body, false, true, schemas.Bedrock, "anthropic.claude-3-5-sonnet", "")
	assert.Equal(t, "bedrock/anthropic.claude-3-5-sonnet", ctx.Value("modelId"))

	// An empty provider leaves the model unprefixed for a later stage to qualify.
	writeModelBack(ctx, body, false, false, "", "gpt-4o", "")
	assert.Equal(t, "gpt-4o", body["model"])
}

// An earlier stage's fallback chain must survive a later stage.
func TestSetFallbacksIfAbsentDoesNotClobber(t *testing.T) {
	body := map[string]any{"fallbacks": []string{"openai/gpt-4o"}}
	assert.False(t, setFallbacksIfAbsent(body, []string{"anthropic/claude-sonnet-4-5"}))
	assert.Equal(t, []string{"openai/gpt-4o"}, body["fallbacks"])

	fresh := map[string]any{}
	assert.True(t, setFallbacksIfAbsent(fresh, []string{"anthropic/claude-sonnet-4-5"}))
	assert.Equal(t, []string{"anthropic/claude-sonnet-4-5"}, fresh["fallbacks"])
	assert.False(t, setFallbacksIfAbsent(map[string]any{}, nil), "an empty chain is not written")
}

func TestApplySemanticRoutingLogsKillSwitchOff(t *testing.T) {
	p := newCapabilityTestPlugin()
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	defer ctx.Cancel()

	body := map[string]any{"model": "openai/gpt-4o"}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, &configstoreTables.TableVirtualKey{Name: "dev"}, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/gpt-4o", out["model"])

	logs := ctx.GetRoutingEngineLogs()
	require.GreaterOrEqual(t, len(logs), 2)
	assert.Equal(t, schemas.RoutingEngineSemantic, logs[0].Engine)
	assert.Contains(t, logs[0].Message, "Selecting model")
	assert.Contains(t, logs[1].Message, "Skipped")
	assert.Contains(t, logs[1].Message, "kill switch off")
	for _, log := range logs {
		assert.NotContains(t, log.Message, "1/extract")
		assert.NotContains(t, log.Message, "1/pool")
		assert.NotContains(t, log.Message, "4/commit")
	}
}

func TestDescribeRankingCapsAtFive(t *testing.T) {
	ranked := make([]routeCandidate, 6)
	for i := range ranked {
		ranked[i] = routeCandidate{Provider: schemas.OpenAI, Model: fmt.Sprintf("m%d", i), score: 1 - float64(i)*0.1}
	}
	got := describeRanking(ranked)
	assert.Contains(t, got, "openai/m0=1.000")
	assert.Contains(t, got, "openai/m4=")
	assert.NotContains(t, got, "openai/m5=")
	assert.True(t, strings.HasSuffix(got, "..."))
}

func routingLogMessages(ctx *schemas.BifrostContext) []string {
	logs := ctx.GetRoutingEngineLogs()
	messages := make([]string, 0, len(logs))
	for _, log := range logs {
		messages = append(messages, log.Message)
	}
	return messages
}

func candidateModels(candidates []routeCandidate) []string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.Model)
	}
	return names
}

func newAccessAlignedSemanticPlugin(t *testing.T, vk *configstoreTables.TableVirtualKey) (*GovernancePlugin, *schemas.BifrostContext) {
	t.Helper()
	p := newCapabilityTestPlugin()
	p.initVLLMSRTransport()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultClass: "general"}
	p.inMemoryStore = &mockInMemoryStore{
		configuredProviders: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.OpenAI: {},
		},
	}

	logger := NewMockLogger()
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*vk},
	}, p.modelCatalog)
	require.NoError(t, err)
	resolver := NewBudgetResolver(store, p.modelCatalog, logger, p.inMemoryStore)
	p.tenantComponents.Store(testTenantID, &tenantGovernanceComponents{
		store:    store,
		resolver: resolver,
	})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyHTTPRequestType, schemas.ChatCompletionRequest)
	return p, ctx
}

// Org blacklist removes a VK-allowed model from the semantic pool; the sibling remains.
func TestEnumerateCandidatesExcludesOrgBlacklistedModel(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testVisionModel, testTextModel}),
		})
	vk.OrgAllowedModelConfigs = []configstoreTables.TableAllowedModelConfig{
		{
			Provider:          "openai",
			AllowedModels:     schemas.WhiteList{"*"},
			BlacklistedModels: schemas.BlackList{testVisionModel},
		},
	}
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()

	comp := p.getComponentsForContext(ctx)
	require.NotNil(t, comp)
	got := candidateModels(p.enumerateCandidates(ctx, comp, vk))
	assert.Equal(t, []string{testTextModel}, got)
}

// Org provider access policy empties the pool for a blocked provider.
func TestEnumerateCandidatesExcludesOrgBlockedProvider(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	vk.OrgProviderAccessPolicy = &configstoreTables.ProviderAccessPolicyRT{
		BlacklistedProviders: schemas.BlackList{"openai"},
	}
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()

	comp := p.getComponentsForContext(ctx)
	require.NotNil(t, comp)
	assert.Empty(t, p.enumerateCandidates(ctx, comp, vk))
}

// Class preferences naming an inaccessible model must not put it in the pool.
func TestEnumerateCandidatesIgnoresPreferenceOnlyModels(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Classes = map[string][]string{"general": {testVisionModel}}

	comp := p.getComponentsForContext(ctx)
	got := candidateModels(p.enumerateCandidates(ctx, comp, vk))
	assert.Equal(t, []string{testTextModel}, got, "preference lists must not expand the VK pool")
}

// When every VK model is access-blocked, semantic declines and leaves the body alone.
func TestApplySemanticRoutingDeclinesWhenAllCandidatesAccessBlocked(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel, testVisionModel}),
		})
	vk.OrgProviderAccessPolicy = &configstoreTables.ProviderAccessPolicyRT{
		BlacklistedProviders: schemas.BlackList{"openai"},
	}
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()

	body := map[string]any{
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testVisionModel, out["model"])
}

// Semantic selection ignores the caller's requested model when choosing from the accessible pool.
func TestApplySemanticRoutingIgnoresRequestedModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testTextModel + `","recommended_models":["openai/` + testTextModel + `"]}`))
	}))
	defer server.Close()

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testVisionModel, testTextModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	// Request the expensive vision model; vLLM-SR returns the cheaper text model.
	body := map[string]any{
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed, "accessible capable pool must route")
	assert.Equal(t, "openai/"+testTextModel, out["model"], "requested model must not bias selection")
}

// Org-blocked original must not be injected as a body fallback after a successful commit.
func TestApplySemanticRoutingOmitsInaccessibleOriginalFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testTextModel + `"}`))
	}))
	defer server.Close()

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	// Org still allows the text model via VK path, but blacklists the vision model the
	// caller asked for — so the original must not appear in fallbacks.
	vk.OrgAllowedModelConfigs = []configstoreTables.TableAllowedModelConfig{
		{
			Provider:          "openai",
			AllowedModels:     schemas.WhiteList{"*"},
			BlacklistedModels: schemas.BlackList{testVisionModel},
		},
	}
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	body := map[string]any{
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed)
	assert.Equal(t, "openai/"+testTextModel, out["model"])
	if fallbacks, ok := out["fallbacks"].([]string); ok {
		for _, fb := range fallbacks {
			assert.NotContains(t, fb, testVisionModel)
		}
	}
}

func TestValidateCandidateRejectsOrgBlockedModel(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	vk.OrgAllowedModelConfigs = []configstoreTables.TableAllowedModelConfig{
		{
			Provider:          "openai",
			AllowedModels:     schemas.WhiteList{"*"},
			BlacklistedModels: schemas.BlackList{testTextModel},
		},
	}
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	comp := p.getComponentsForContext(ctx)

	candidate := routeCandidate{
		Provider: schemas.OpenAI,
		Model:    testTextModel,
		Config:   vk.AllowedModelConfigs[0],
	}
	_, ok := p.validateCandidate(comp, vk, candidate)
	assert.False(t, ok)
}

func TestShouldApplySemanticRoutingDefaultForAll(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: true}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}

	run, pref, handoff := p.shouldApplySemanticRouting(nil, vk, nil, true)
	assert.True(t, run)
	assert.Nil(t, pref)
	assert.Equal(t, "default semantic routing", handoff)
}

func TestShouldApplySemanticRoutingPinOverridesDefault(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: true}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}
	pin := &RoutingDecision{MatchedRuleName: "Token-Limits", Provider: "openai", Model: "gpt-4o"}

	run, pref, handoff := p.shouldApplySemanticRouting(nil, vk, pin, true)
	assert.False(t, run)
	assert.Nil(t, pref)
	assert.Empty(t, handoff)
}

func TestShouldApplySemanticRoutingFlagOffRequiresSemanticRule(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: false}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}

	run, _, _ := p.shouldApplySemanticRouting(nil, vk, nil, true)
	assert.False(t, run, "no rule and default_for_all=false must not run semantic")

	sem := &RoutingDecision{
		MatchedRuleName: "Pick capable",
		SemanticRouting: true,
		Fallbacks:       []string{"openai/gpt-4o"},
	}
	run, pref, handoff := p.shouldApplySemanticRouting(nil, vk, sem, true)
	assert.True(t, run)
	assert.Equal(t, []string{"openai/gpt-4o"}, pref)
	assert.Contains(t, handoff, "Pick capable")
}

func TestShouldApplySemanticRoutingNoVirtualKey(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: true}
	sem := &RoutingDecision{
		MatchedRuleName: "Pick capable",
		SemanticRouting: true,
	}

	run, _, _ := p.shouldApplySemanticRouting(nil, nil, nil, true)
	assert.False(t, run)

	run, _, _ = p.shouldApplySemanticRouting(nil, nil, sem, true)
	assert.False(t, run)
}

func TestShouldApplySemanticRoutingKillSwitchOff(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: false, DefaultForAll: true}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}

	run, _, _ := p.shouldApplySemanticRouting(nil, vk, nil, true)
	assert.False(t, run)
}

func TestApplySemanticRoutingOptOutHeader(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: true}
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	defer ctx.Cancel()

	body := map[string]any{"model": "openai/" + testTextModel}
	req := &schemas.HTTPRequest{
		Headers: map[string]string{semanticRoutingDisableHeader: "true"},
	}
	assert.True(t, semanticRoutingOptedOut(req))

	out, routed := p.applySemanticRouting(ctx, req, body, &configstoreTables.TableVirtualKey{Name: "dev"}, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testTextModel, out["model"])
	messages := routingLogMessages(ctx)
	require.NotEmpty(t, messages)
	assert.Contains(t, strings.Join(messages, "\n"), "disabled by request header")
}

func TestShouldApplySemanticRoutingOmittedModel(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: false}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}

	run, pref, handoff := p.shouldApplySemanticRouting(nil, vk, nil, false)
	assert.True(t, run, "omitted model is an extra trigger when default_for_all is off")
	assert.Nil(t, pref)
	assert.Equal(t, "missing model; semantic routing", handoff)

	run, _, _ = p.shouldApplySemanticRouting(nil, vk, nil, true)
	assert.False(t, run, "with a model present and default_for_all off, existing path must not run")
}

func TestShouldApplySemanticRoutingOmittedModelPinStillWins(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: false}
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}
	pin := &RoutingDecision{MatchedRuleName: "Token-Limits", Provider: "openai", Model: "gpt-4o"}

	run, _, _ := p.shouldApplySemanticRouting(nil, vk, pin, false)
	assert.False(t, run)
}

func TestShouldApplySemanticRoutingOmittedModelNoVKAndKillSwitch(t *testing.T) {
	p := newCapabilityTestPlugin()
	p.semanticRouting = &SemanticRoutingConfig{Enabled: true, DefaultForAll: false}

	run, _, _ := p.shouldApplySemanticRouting(nil, nil, nil, false)
	assert.False(t, run)

	p.semanticRouting.Enabled = false
	vk := &configstoreTables.TableVirtualKey{Name: "dev"}
	run, _, _ = p.shouldApplySemanticRouting(nil, vk, nil, false)
	assert.False(t, run)
}

func TestApplySemanticRoutingOmittedModelSelectsFromPool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testTextModel + `"}`))
	}))
	defer server.Close()

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel, testVisionModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed)
	assert.Equal(t, "openai/"+testTextModel, out["model"])
	_, hasFallbacks := out["fallbacks"]
	if hasFallbacks {
		fallbacks, _ := out["fallbacks"].([]string)
		for _, fb := range fallbacks {
			assert.NotEqual(t, "", fb)
			assert.NotContains(t, fb, "//", "omitted original must not be injected as a fallback")
		}
	}
}

func TestApplySemanticRoutingOmittedModelEmptyPoolDoesNotInventModel(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Empty", nil)
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()

	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed)
	_, hasModel := out["model"]
	assert.False(t, hasModel)
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "no model on request and semantic did not select")
}

func TestTierMatchScoreMidBand(t *testing.T) {
	assert.Equal(t, 0.7, tierMatchScore(&modelcatalog.PricingEntry{CapabilityTier: modelcatalog.CapabilityTierFlagship}))
	assert.Equal(t, 1.0, tierMatchScore(&modelcatalog.PricingEntry{CapabilityTier: modelcatalog.CapabilityTierMid}))
	assert.Equal(t, 0.5, tierMatchScore(&modelcatalog.PricingEntry{CapabilityTier: modelcatalog.CapabilityTierSmall}))
}

func TestRankPrefersCheaperWhenNoPreference(t *testing.T) {
	logger := NewMockLogger()
	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.SeedForTest(logger, []modelcatalog.TestModel{
		{
			Provider: schemas.OpenAI, Model: "cheap-small",
			ResponseTypes:   []string{string(schemas.ChatCompletionRequest)},
			SupportedParams: []string{"tools"},
			MaxInputTokens:  intPtr(128000), MaxOutputTokens: intPtr(4096),
			InputCostPerToken: floatPtr(0.0000001), OutputCostPerToken: floatPtr(0.0000004),
			Categories: []string{"Code Generation"}, CapabilityTier: modelcatalog.CapabilityTierSmall,
		},
		{
			Provider: schemas.OpenAI, Model: "reasoning-flagship",
			ResponseTypes:   []string{string(schemas.ChatCompletionRequest)},
			SupportedParams: []string{"tools", "reasoning"},
			MaxInputTokens:  intPtr(128000), MaxOutputTokens: intPtr(4096),
			InputCostPerToken: floatPtr(0.00001), OutputCostPerToken: floatPtr(0.00003),
			Categories: []string{"Code Generation"}, CapabilityTier: modelcatalog.CapabilityTierFlagship,
			SupportsReasoning: boolPtr(true),
		},
	})
	p := &GovernancePlugin{modelCatalog: catalog, logger: logger}
	profile := buildRequestProfile(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "Write a concurrent Go scheduler"}},
	})
	cfg := &SemanticRoutingConfig{Enabled: true, PreferenceWeight: floatPtr(0), TaskWeight: floatPtr(0), TierWeight: floatPtr(0)}
	ranked := p.rankCandidates(testCandidates("cheap-small", "reasoning-flagship"), profile, cfg, nil)
	require.NotEmpty(t, ranked)
	assert.Equal(t, "cheap-small", ranked[0].Model)
}

func TestNoLocalRouterWhenVLLMSRUnsetDeclines(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Classifier = &DeprecatedClassifierConfig{BaseURL: "http://127.0.0.1:1", TimeoutMs: 50}

	body := map[string]any{
		"model":    "openai/" + testTextModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testTextModel, out["model"])

	joined := strings.Join(routingLogMessages(ctx), "\n")
	assert.Contains(t, joined, "2/route: layer2 router unset; declining semantic rewrite")
	assert.NotContains(t, joined, "2/classify")
}
