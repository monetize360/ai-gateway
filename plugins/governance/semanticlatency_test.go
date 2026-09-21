package governance

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSemanticRoutingDeclinePathLatencyMeasurement reports the wall-clock cost of
// Layer 1 + declining when vLLM-SR is unset. Bound is loose for shared CI boxes.
func TestSemanticRoutingDeclinePathLatencyMeasurement(t *testing.T) {
	require.Equal(t, 10*time.Millisecond, semanticRoutingBudget, "configured hard decision budget")

	vk := buildVirtualKeyWithProviders("vk1", "sk-bf-test", "Test",
		[]configstoreTables.TableVirtualKeyProviderConfig{
			buildProviderConfig("openai", []string{testTextModel, testVisionModel}),
		})
	p, ctx := newAccessAlignedSemanticPlugin(t, vk)
	defer ctx.Cancel()

	const runs = 200
	samples := make([]time.Duration, 0, runs)
	for range runs {
		body := map[string]any{
			"model":    "openai/" + testTextModel,
			"messages": []any{map[string]any{"role": "user", "content": "Implement a Python function for binary search"}},
		}
		runCtx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
		runCtx.SetValue(schemas.BifrostContextKeyHTTPRequestType, schemas.ChatCompletionRequest)
		start := time.Now()
		_, routed := p.applySemanticRouting(runCtx, &schemas.HTTPRequest{}, body, vk, nil)
		samples = append(samples, time.Since(start))
		runCtx.Cancel()
		require.False(t, routed, "without router.base_url semantic must decline")
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("semantic decline-path latency over %d runs: p50=%s p95=%s max=%s",
		runs, samples[runs/2], samples[runs*95/100], samples[runs-1])

	assert.Less(t, samples[runs*95/100], 50*time.Millisecond,
		"p95 far above this would mean the decline path is doing unexpected I/O")
}

// TestVLLMSRPreviewClientLatencyMeasurement measures the synchronous sidecar
// client against a fake server with a synthetic 25ms delay. The sidecar has its
// own configured timeout; semanticRoutingBudget continues to bound local work.
func TestVLLMSRPreviewClientLatencyMeasurement(t *testing.T) {
	const serverDelay = 25 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		_, _ = w.Write([]byte(`{"selected_model":"openai/` + testVisionModel + `"}`))
	}))
	defer server.Close()

	p := newCapabilityTestPlugin()
	p.initVLLMSRTransport()
	cfg := &SemanticRoutingConfig{
		Enabled: true,
		Router:  &VLLMSRRouterConfig{BaseURL: server.URL, TimeoutMs: 2000},
	}
	require.True(t, cfg.routerEnabled())

	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	start := time.Now()
	route, err := p.previewVLLMSRRoute(nil, body, buildRequestProfile(body), testCandidates(testTextModel, testVisionModel), cfg)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, "openai/"+testVisionModel, route.SelectedModel)

	t.Logf("vllm-sr preview client took %s against a fake server delayed by %s", elapsed, serverDelay)
	assert.Greater(t, elapsed, semanticRoutingBudget,
		"the sidecar timeout is separate from the local routing budget")
}
