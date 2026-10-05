package governance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/plugins/semanticrouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVLLMSRPreviewFlexibleJSON(t *testing.T) {
	route, err := parseVLLMSRPreview([]byte(`{
		"selected_model": "gemini/gemini-2.5-flash",
		"selected_decision": "gemini-pool",
		"algorithm": {"type": "multi_factor"},
		"selection_status": "ok",
		"candidates": [{"model": "gemini/gemini-2.5-pro"}, "gemini/gemini-2.5-flash-lite"]
	}`))
	require.NoError(t, err)
	assert.Equal(t, "gemini/gemini-2.5-flash", route.SelectedModel)
	assert.Equal(t, "gemini-pool", route.Decision)
	assert.Equal(t, "multi_factor", route.Algorithm)
	assert.Equal(t, []string{"gemini/gemini-2.5-pro", "gemini/gemini-2.5-flash-lite"}, route.Candidates)
	assert.False(t, route.skipRewrite())
}

func TestParseVLLMSRPreviewCurrentAPI(t *testing.T) {
	route, err := parseVLLMSRPreview([]byte(`{
		"selected_model": "gemini/gemini-2.5-flash",
		"recommended_models": [
			"gemini/gemini-2.5-flash",
			"gemini/gemini-2.5-pro"
		],
		"routing_decision": "gemini-pool",
		"decision_result": {
			"decision_name": "gemini-pool",
			"algorithm": "multi_factor"
		},
		"selection_status": "selected",
		"selection_method": "multi_factor"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "gemini/gemini-2.5-flash", route.SelectedModel)
	assert.Equal(t, "gemini-pool", route.Decision)
	assert.Equal(t, "multi_factor", route.Algorithm)
	assert.Equal(t, []string{"gemini/gemini-2.5-flash", "gemini/gemini-2.5-pro"}, route.Candidates)
}

func TestBuildVLLMSRPreviewRequestUsesStrictAPIShape(t *testing.T) {
	body := map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "hello"}},
		"tools":       []any{map[string]any{"type": "function"}},
		"temperature": 0.2,
	}
	req := buildVLLMSRPreviewRequest(body, buildRequestProfile(body), testCandidates(testTextModel), &VLLMSRRouterConfig{})

	assert.Equal(t, defaultVLLMSREntrypoint, req["model"])
	assert.Contains(t, req, "messages")
	assert.Contains(t, req, "tools")
	assert.NotContains(t, req, "eligible_models")
	assert.NotContains(t, req, "candidates")
	assert.NotContains(t, req, "request_type")
	assert.NotContains(t, req, "request_profile")
	assert.NotContains(t, req, "temperature")
}

func TestBuildVLLMSRPreviewRequestKeepsAttachmentParts(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is in this picture?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url":    "data:image/png;base64," + strings.Repeat("A", 4096),
					"detail": "high",
				}},
			},
		}},
	}
	req := buildVLLMSRPreviewRequest(body, buildRequestProfile(body), testCandidates(testVisionModel), &VLLMSRRouterConfig{})

	messages, ok := req["messages"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, messages, 1)

	parts, ok := messages[0]["content"].([]any)
	require.True(t, ok, "image content must stay structured so input_modality signals can fire")
	require.Len(t, parts, 2)

	image, ok := parts[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image_url", image["type"])

	inner, ok := image["image_url"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "data:inline", inner["url"], "inline media bytes must not be shipped to the sidecar")
	assert.Equal(t, "high", inner["detail"])
}

func TestBuildVLLMSRPreviewRequestFlattensTextOnlyParts(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "text", "text": "second"},
			},
		}},
	}
	req := buildVLLMSRPreviewRequest(body, buildRequestProfile(body), testCandidates(testTextModel), &VLLMSRRouterConfig{})

	messages, ok := req["messages"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	assert.Equal(t, "first\nsecond", messages[0]["content"])
}

func TestVLLMSRFastResponseSkipsRewrite(t *testing.T) {
	route, err := parseVLLMSRPreview([]byte(`{
		"selection_status": "not_required",
		"selection_method": "fast_response"
	}`))
	require.NoError(t, err)
	assert.True(t, route.skipRewrite())
}

func TestVLLMSRProfileOnlySkipsRewrite(t *testing.T) {
	route := &vllmsrRoute{SelectionStatus: "profile_only", SelectionMethod: "capability_profile"}
	assert.True(t, route.skipRewrite())
}

func TestIntersectRouteDropsModelsOutsideL1(t *testing.T) {
	eligible := testCandidates(testTextModel, testVisionModel)
	ordered := intersectRouteWithEligible(eligible, &vllmsrRoute{
		SelectedModel: "openai/not-in-pool",
		Candidates:    []string{"openai/" + testVisionModel, "openai/" + testTextModel},
	})
	require.Len(t, ordered, 2)
	assert.Equal(t, testVisionModel, ordered[0].Model)
	assert.Equal(t, testTextModel, ordered[1].Model)
}

func TestIntersectEmptyWhenNoneMatchL1(t *testing.T) {
	eligible := testCandidates(testTextModel)
	ordered := intersectRouteWithEligible(eligible, &vllmsrRoute{SelectedModel: "gemini/gemini-2.5-pro"})
	assert.Nil(t, ordered)
}

func TestApplySemanticRoutingVLLMSRConfiguredUsesRouterWinner(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		payload, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var request map[string]any
		require.NoError(t, json.Unmarshal(payload, &request))
		assert.Equal(t, defaultVLLMSREntrypoint, request["model"])
		assert.NotContains(t, request, "eligible_models")
		assert.NotContains(t, request, "request_profile")
		_, _ = w.Write([]byte(`{
			"selected_model":"openai/` + testVisionModel + `",
			"recommended_models":["openai/` + testVisionModel + `","openai/` + testTextModel + `"],
			"routing_decision":"vision",
			"selection_method":"multi_factor",
			"selection_status":"selected"
		}`))
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
		"model": "openai/" + testTextModel,
		"messages": []any{
			map[string]any{"role": "user", "content": "describe this image", "extra": "ignored"},
		},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed)
	assert.Equal(t, "openai/"+testVisionModel, out["model"])
	assert.Equal(t, int32(1), hits.Load())
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "2/route: layer2 selected=openai/"+testVisionModel)
}

func TestApplySemanticRoutingIgnoresOutOfPoolSidecarAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/not-in-pool"}`))
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
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed, "out-of-pool sidecar answer must not fall back to a local heuristic router")
	assert.Equal(t, "openai/"+testVisionModel, out["model"], "incoming model must be kept")
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "recommendations do not overlap the VK pool; declining semantic rewrite")
}

func TestApplySemanticRoutingSlowVLLMSRTimesOutAndDeclines(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testVisionModel + `"}`))
	}))
	defer server.Close()

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel, testVisionModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 20}

	body := map[string]any{
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	start := time.Now()
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	elapsed := time.Since(start)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testVisionModel, out["model"])
	assert.Equal(t, 10*time.Millisecond, semanticRoutingBudget, "configured decision deadline")
	assert.Equal(t, int32(1), hits.Load())
	assert.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
	assert.Less(t, elapsed, 100*time.Millisecond, "CI wall time allows scheduler tolerance; the configured budget remains 10ms")
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "layer2 failed")
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "declining semantic rewrite")
}

func TestApplySemanticRoutingVLLMSRFastResponseKeepsIncoming(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"selection_status":"not_required","selection_method":"fast_response"}`))
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
		"model":    "openai/" + testVisionModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testVisionModel, out["model"])
	assert.Equal(t, int32(1), hits.Load())
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "no model rewrite required")
}

func TestApplySemanticRoutingRouterSetSkipsNVIDIA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testTextModel + `"}`))
	}))
	defer server.Close()

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
	p.semanticRouting.Classifier = &DeprecatedClassifierConfig{BaseURL: "http://127.0.0.1:1", TimeoutMs: 50}

	body := map[string]any{
		"model":    "openai/" + testTextModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	_, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed)
	joined := strings.Join(routingLogMessages(ctx), "\n")
	assert.NotContains(t, joined, "2/classify:")
	assert.Contains(t, joined, "2/route:")
}

func TestParseVLLMSRPreviewRejectsMissingAndMalformed(t *testing.T) {
	_, err := parseVLLMSRPreview([]byte(`{"selection_status":"selected","selection_method":"multi_factor"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing selected_model")

	_, err = parseVLLMSRPreview([]byte(`not-json`))
	require.Error(t, err)
}

func TestPreviewVLLMSRRouteSendsAuthAndRejectsNon2xx(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		http.Error(w, `{"error":"nope"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	p := &GovernancePlugin{}
	p.initVLLMSRTransport()
	cfg := &SemanticRoutingConfig{
		Router: &VLLMSRRouterConfig{
			BaseURL:   server.URL,
			APIKey:    "secret-token",
			TimeoutMs: 500,
		},
	}
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	_, err := p.previewVLLMSRRoute(nil, body, buildRequestProfile(body), testCandidates(testTextModel), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http status 502")
	assert.Equal(t, "Bearer secret-token", gotAuth)
	_, _, failOpen, _, _ := p.SemanticRouteStats()
	assert.GreaterOrEqual(t, failOpen, uint64(1))
}

func TestPreviewVLLMSRRouteEmptySelectedFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selection_status":"selected"}`))
	}))
	defer server.Close()

	p := &GovernancePlugin{}
	p.initVLLMSRTransport()
	cfg := &SemanticRoutingConfig{Router: &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 500}}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	_, err := p.previewVLLMSRRoute(nil, body, buildRequestProfile(body), testCandidates(testTextModel), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing selected_model")
}

func TestPreviewVLLMSRCircuitOpensAfterFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	p := &GovernancePlugin{}
	p.initVLLMSRTransport()
	cfg := &SemanticRoutingConfig{Router: &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 200}}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	eligible := testCandidates(testTextModel)
	profile := buildRequestProfile(body)

	for i := 0; i < vllmsrBreakerThreshold; i++ {
		_, err := p.previewVLLMSRRoute(nil, body, profile, eligible, cfg)
		require.Error(t, err)
	}
	_, err := p.previewVLLMSRRoute(nil, body, profile, eligible, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit open")
}

func TestVLLMSRRouterPreviewURLParsing(t *testing.T) {
	cfg := &VLLMSRRouterConfig{BaseURL: "https://vllm.example.com/", PreviewPath: "api/v1/routing/preview"}
	assert.Equal(t, "https://vllm.example.com/api/v1/routing/preview", cfg.previewURL())
	assert.True(t, strings.HasPrefix(cfg.previewURL(), "https://"))
}

func TestValidateRouterBaseURL(t *testing.T) {
	t.Setenv("BIFROST_ENV", "test")
	err := (&SemanticRoutingConfig{Router: &VLLMSRRouterConfig{BaseURL: "127.0.0.1:18080"}}).validateAndNormalizeSemanticRouting(nil)
	require.Error(t, err)

	err = (&SemanticRoutingConfig{Router: &VLLMSRRouterConfig{BaseURL: "ftp://example.com"}}).validateAndNormalizeSemanticRouting(nil)
	require.Error(t, err)

	err = (&SemanticRoutingConfig{Router: &VLLMSRRouterConfig{BaseURL: "http://127.0.0.1:18080", TimeoutMs: 50}}).validateAndNormalizeSemanticRouting(nil)
	require.NoError(t, err)

	err = (&SemanticRoutingConfig{Router: &VLLMSRRouterConfig{Mode: routerModePlugin}}).validateAndNormalizeSemanticRouting(nil)
	require.NoError(t, err)

	err = (&SemanticRoutingConfig{Router: &VLLMSRRouterConfig{Mode: routerModeHTTP}}).validateAndNormalizeSemanticRouting(nil)
	require.Error(t, err)
}

type stubLayer2Router struct {
	result *semanticrouter.PreviewResult
	err    error
	hits   atomic.Int32
	last   semanticrouter.PreviewInput
}

func (s *stubLayer2Router) Preview(_ context.Context, input semanticrouter.PreviewInput) (*semanticrouter.PreviewResult, error) {
	s.hits.Add(1)
	s.last = input
	return s.result, s.err
}

type deadlineLayer2Router struct{}

func (deadlineLayer2Router) Preview(ctx context.Context, _ semanticrouter.PreviewInput) (*semanticrouter.PreviewResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestApplySemanticRoutingPluginModeUsesInjectedRouter(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel, testVisionModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	stub := &stubLayer2Router{
		result: &semanticrouter.PreviewResult{
			SelectedModel:   "openai/" + testVisionModel,
			Candidates:      []string{"openai/" + testVisionModel, "openai/" + testTextModel},
			Decision:        "vision",
			Algorithm:       "static",
			SelectionStatus: "selected",
			SelectionMethod: "static",
		},
	}
	p.SetSemanticLayer2(stub)
	p.semanticRouting.Router = &VLLMSRRouterConfig{Mode: routerModePlugin}

	body := map[string]any{
		"model":    "openai/" + testTextModel,
		"messages": []any{map[string]any{"role": "user", "content": "describe this image"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	require.True(t, routed)
	assert.Equal(t, "openai/"+testVisionModel, out["model"])
	assert.Equal(t, int32(1), stub.hits.Load())
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "2/route: layer2 selected=openai/"+testVisionModel)
}

func TestPreviewPluginRouteForwardsCompleteRoutingFacts(t *testing.T) {
	p := &GovernancePlugin{}
	stub := &stubLayer2Router{result: &semanticrouter.PreviewResult{
		SelectedModel:   "openai/" + testTextModel,
		Candidates:      []string{"openai/" + testTextModel},
		SelectionStatus: "selected",
		SelectionMethod: "static",
	}}
	p.SetSemanticLayer2(stub)
	cfg := &SemanticRoutingConfig{Router: &VLLMSRRouterConfig{
		Mode:       routerModePlugin,
		Entrypoint: "vllm-sr/custom",
		TimeoutMs:  100,
	}}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()
	body := map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "use a tool"}},
		"tools":           []any{map[string]any{"type": "function"}},
		"tool_choice":     "auto",
		"response_format": map[string]any{"type": "json_object"},
		"max_tokens":      128,
		"metadata":        map[string]any{"tenant": "one"},
		"preview_context": map[string]any{"session_id": "session-one"},
	}

	route, err := p.previewPluginRoute(ctx, body, &RequestProfile{}, nil, cfg)
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, "vllm-sr/custom", stub.last.Model)
	assert.Len(t, stub.last.Messages, 1)
	assert.NotNil(t, stub.last.Tools)
	assert.Equal(t, "auto", stub.last.ToolChoice)
	assert.Equal(t, 128, stub.last.MaxTokens)
	assert.Equal(t, "one", stub.last.Metadata["tenant"])
	assert.Equal(t, "session-one", stub.last.PreviewContext["session_id"])
}

func TestPreviewPluginRouteHonorsTimeoutAndRecordsIt(t *testing.T) {
	p := &GovernancePlugin{}
	p.SetSemanticLayer2(deadlineLayer2Router{})
	cfg := &SemanticRoutingConfig{Router: &VLLMSRRouterConfig{
		Mode:      routerModePlugin,
		TimeoutMs: 1,
	}}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()

	_, err := p.previewPluginRoute(ctx, map[string]any{}, &RequestProfile{}, nil, cfg)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, _, _, timedOut, _ := p.SemanticRouteStats()
	assert.Equal(t, uint64(1), timedOut)
}

func TestApplySemanticRoutingCallsLayer2BeforeCatalogFiltering(t *testing.T) {
	const unknownModel = "model-not-in-catalog"
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{unknownModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	stub := &stubLayer2Router{
		result: &semanticrouter.PreviewResult{
			SelectedModel:   "openai/" + unknownModel,
			Candidates:      []string{"openai/" + unknownModel},
			Decision:        "unknown-model-test",
			Algorithm:       "static",
			SelectionStatus: "selected",
			SelectionMethod: "static",
		},
	}
	p.SetSemanticLayer2(stub)
	p.semanticRouting.Router = &VLLMSRRouterConfig{Mode: routerModePlugin}

	body := map[string]any{
		"model":    "openai/" + unknownModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)

	assert.False(t, routed, "post-router capability validation must still reject an unknown model")
	assert.Equal(t, "openai/"+unknownModel, out["model"])
	assert.Equal(t, int32(1), stub.hits.Load(), "catalog filtering must not prevent the Layer 2 call")
	logs := strings.Join(routingLogMessages(ctx), "\n")
	assert.Contains(t, logs, "2/route: calling semantic-router plugin")
	assert.Contains(t, logs, "3/filter: 0/1 router recommendations capable")
	assert.Contains(t, logs, "no catalog entry")
}

func TestApplySemanticRoutingPluginModeWithoutInjectionDeclines(t *testing.T) {
	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{Mode: routerModePlugin}

	body := map[string]any{
		"model":    "openai/" + testTextModel,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	assert.False(t, routed)
	assert.Equal(t, "openai/"+testTextModel, out["model"])
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "layer2 router unset")
}

func expectedAutoRankedModel(t *testing.T, p *GovernancePlugin, ctx *schemas.BifrostContext, vk *configstoreTables.TableVirtualKey, body map[string]any) string {
	t.Helper()
	comp := p.getComponentsForContext(ctx)
	require.NotNil(t, comp)
	pool := p.enumerateCandidates(ctx, comp, vk)
	require.NotEmpty(t, pool)
	ranked := p.rankCandidates(pool, buildRequestProfile(body), p.semanticRouting, nil)
	require.NotEmpty(t, ranked)
	return ranked[0].qualified()
}

func helloMessagesBody(model string) map[string]any {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	if model != "" {
		body["model"] = model
	}
	return body
}

func layer2VisionTextVK() configstoreTables.TableVirtualKey {
	return *buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testVisionModel, testTextModel}),
		})
}

func assertExplicitKeepsIncoming(t *testing.T, p *GovernancePlugin, ctx *schemas.BifrostContext, vk *configstoreTables.TableVirtualKey, incoming string) {
	t.Helper()
	body := helloMessagesBody(incoming)
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	joined := strings.Join(routingLogMessages(ctx), "\n")
	assert.False(t, routed)
	assert.Equal(t, incoming, out["model"])
	assert.NotContains(t, joined, "Auto ranking VK pool")
	assert.Contains(t, joined, "declining semantic rewrite")
}

func assertAutoRanksVKPool(t *testing.T, p *GovernancePlugin, ctx *schemas.BifrostContext, vk *configstoreTables.TableVirtualKey) {
	t.Helper()
	body := helloMessagesBody("")
	want := expectedAutoRankedModel(t, p, ctx, vk, body)
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, vk, nil)
	joined := strings.Join(routingLogMessages(ctx), "\n")
	require.True(t, routed)
	assert.Equal(t, want, out["model"])
	assert.Contains(t, joined, "Auto ranking VK pool")
	assert.Contains(t, joined, "layer2 failed")
}

func TestApplySemanticRoutingLayer2ErrorKeepsIncomingAndAutoRanks(t *testing.T) {
	incoming := "openai/" + testVisionModel
	vk := layer2VisionTextVK()

	t.Run("explicit 502 keeps incoming", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"nope"}`, http.StatusBadGateway)
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertExplicitKeepsIncoming(t, p, ctx, &vk, incoming)
	})

	t.Run("auto 502 ranks VK pool", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"nope"}`, http.StatusBadGateway)
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertAutoRanksVKPool(t, p, ctx, &vk)
	})

	t.Run("auto timeout ranks VK pool", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"selected_model":"openai/` + testVisionModel + `"}`))
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 20}
		assertAutoRanksVKPool(t, p, ctx, &vk)
	})

	t.Run("explicit decode keeps incoming", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertExplicitKeepsIncoming(t, p, ctx, &vk, incoming)
	})

	t.Run("auto decode ranks VK pool", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertAutoRanksVKPool(t, p, ctx, &vk)
	})

	t.Run("explicit empty selected keeps incoming", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"selection_status":"selected"}`))
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertExplicitKeepsIncoming(t, p, ctx, &vk, incoming)
	})

	t.Run("auto empty selected ranks VK pool", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"selection_status":"selected"}`))
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}
		assertAutoRanksVKPool(t, p, ctx, &vk)
	})

	t.Run("explicit circuit open keeps incoming", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 200}
		tripVLLMSRCircuit(t, p)
		assertExplicitKeepsIncoming(t, p, ctx, &vk, incoming)
	})

	t.Run("auto circuit open ranks VK pool", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
		defer ctx.Cancel()
		p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 200}
		tripVLLMSRCircuit(t, p)
		assertAutoRanksVKPool(t, p, ctx, &vk)
	})
}

func tripVLLMSRCircuit(t *testing.T, p *GovernancePlugin) {
	t.Helper()
	cfg := p.semanticRouting
	body := helloMessagesBody("")
	profile := buildRequestProfile(body)
	eligible := testCandidates(testTextModel)
	for i := 0; i < vllmsrBreakerThreshold; i++ {
		_, err := p.previewVLLMSRRoute(nil, body, profile, eligible, cfg)
		require.Error(t, err)
	}
	_, err := p.previewVLLMSRRoute(nil, body, profile, eligible, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit open")
}

func TestApplySemanticRoutingLayer2ErrorAutoMatchesRankCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	vk := layer2VisionTextVK()
	p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	comp := p.getComponentsForContext(ctx)
	require.NotNil(t, comp)
	pool := p.enumerateCandidates(ctx, comp, &vk)
	require.GreaterOrEqual(t, len(pool), 2)
	assert.Equal(t, testVisionModel, pool[0].Model, "VK list order must put vision first")

	body := helloMessagesBody("")
	want := expectedAutoRankedModel(t, p, ctx, &vk, body)
	assert.Equal(t, "openai/"+testTextModel, want, "cost ranking must prefer text over first VK row")

	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, &vk, nil)
	require.True(t, routed)
	assert.Equal(t, want, out["model"])
	assert.NotEqual(t, "openai/"+testVisionModel, out["model"])
	assert.Contains(t, strings.Join(routingLogMessages(ctx), "\n"), "Auto ranking VK pool")
}

func TestApplySemanticRoutingOmittedModelOutOfPoolDoesNotRank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selected_model":"openai/not-in-pool"}`))
	}))
	defer server.Close()

	vk := layer2VisionTextVK()
	p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	body := helloMessagesBody("")
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, &vk, nil)
	assert.False(t, routed)
	_, hasModel := out["model"]
	assert.False(t, hasModel)
	joined := strings.Join(routingLogMessages(ctx), "\n")
	assert.Contains(t, joined, "recommendations do not overlap the VK pool; declining semantic rewrite")
	assert.NotContains(t, joined, "Auto ranking VK pool")
}

func TestApplySemanticRoutingOmittedModelFastResponseDoesNotRank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selection_status":"not_required","selection_method":"fast_response"}`))
	}))
	defer server.Close()

	vk := layer2VisionTextVK()
	p, ctx := newAccessAlignedSemanticPlugin(t, &vk)
	defer ctx.Cancel()
	p.semanticRouting.Router = &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 1000}

	body := helloMessagesBody("")
	out, routed := p.applySemanticRouting(ctx, &schemas.HTTPRequest{}, body, &vk, nil)
	assert.False(t, routed)
	_, hasModel := out["model"]
	assert.False(t, hasModel)
	joined := strings.Join(routingLogMessages(ctx), "\n")
	assert.Contains(t, joined, "no model rewrite required")
	assert.NotContains(t, joined, "Auto ranking VK pool")
}
