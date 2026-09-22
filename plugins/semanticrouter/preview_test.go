//go:build cgo

package semanticrouter_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/maximhq/bifrost/plugins/semanticrouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recipePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "plugins", "governance", "vllm_sr", "config.yaml")
}

func newPlugin(t *testing.T) *semanticrouter.Plugin {
	t.Helper()
	p, err := semanticrouter.Init(&semanticrouter.Config{
		RecipeFile: recipePath(t),
		Entrypoint: "vllm-sr/auto",
	}, nil)
	require.NoError(t, err)
	return p
}

func preview(t *testing.T, p *semanticrouter.Plugin, content any) *semanticrouter.PreviewResult {
	t.Helper()
	out, err := p.Preview(context.Background(), semanticrouter.PreviewInput{
		Messages: []map[string]any{{"role": "user", "content": content}},
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	return out
}

func TestPreviewCodePool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Write a Python function that reverses a linked list.")
	assert.Equal(t, "code", out.Decision)
	assert.Equal(t, "anthropic/claude-3.5-sonnet", out.SelectedModel)
	assert.Equal(t, "selected", out.SelectionStatus)
	assert.Equal(t, []string{
		"anthropic/claude-3.5-sonnet",
		"nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4",
		"deepseek/deepseek-r1-distill-32b",
		"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4",
		"fakellm-qwen/qwen2.5-72b-instruct",
		"meta/llama-3.1-70b-instruct",
		"gemini/gemini-2.5-pro",
		"gemini/gemini-2.5-flash",
	}, out.Candidates)
}

func TestPreviewGeneralPool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Summarize this PR in three bullets.")
	assert.Equal(t, "general", out.Decision)
	assert.Equal(t, "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4", out.SelectedModel)
}

func TestPreviewDeepReasoningPool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Prove the theorem and derive the architecture trade-offs step by step.")
	assert.Equal(t, "deep-reasoning", out.Decision)
	assert.Equal(t, "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4", out.SelectedModel)
}

func TestPreviewImagePool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, []any{
		map[string]any{"type": "text", "text": "What is in this picture?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:inline"}},
	})
	assert.Equal(t, "vision", out.Decision)
	assert.Equal(t, "fakellm-openai/gpt-4o", out.SelectedModel)
}

func TestPreviewVideoPool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, []any{
		map[string]any{"type": "text", "text": "Summarize this video."},
		map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}},
	})
	assert.Equal(t, "video", out.Decision)
	// The current OpenAI-chat wire codec cannot carry video input. Capability
	// admission therefore rejects the card instead of dispatching an invalid
	// request as selected, while preserving the decision's recommendations.
	assert.Empty(t, out.SelectedModel)
	assert.Equal(t, []string{
		"qwen/qwen2.5-vl-72b-instruct",
	}, out.Candidates)
}

func TestPreviewCatchAllPool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Hello, how are you today?")
	assert.Equal(t, "general", out.Decision)
	assert.Equal(t, "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4", out.SelectedModel)
	assert.Equal(t, "static", out.Algorithm)
	assert.Len(t, out.Candidates, 10)
}

func TestPreviewSimplePool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Answer briefly in one sentence: what is HTTP?")
	assert.Equal(t, "simple", out.Decision)
	assert.Equal(t, "meta/llama-3.1-8b-instruct", out.SelectedModel)
}

func TestPreviewFactualQAPool(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "What is HTTP?")
	assert.Equal(t, "simple", out.Decision)
	assert.Equal(t, "meta/llama-3.1-8b-instruct", out.SelectedModel)
}

func TestPreviewToolsPool(t *testing.T) {
	p := newPlugin(t)
	out, err := p.Preview(context.Background(), semanticrouter.PreviewInput{
		Messages: []map[string]any{{"role": "user", "content": "Look up the weather and summarize it."}},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get weather",
				"parameters":  map[string]any{"type": "object"},
			},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "tools", out.Decision)
	assert.Equal(t, "nvidia/nemotron-ultra", out.SelectedModel)
}

func TestPreviewLongContextPool(t *testing.T) {
	p := newPlugin(t)
	// 1500 tokens ≈ 6000 chars
	long := make([]byte, 6200)
	for i := range long {
		long[i] = 'a'
	}
	out := preview(t, p, string(long)+" please help")
	assert.Equal(t, "long-context", out.Decision)
	assert.Equal(t, "gemini/gemini-3.5-flash-lite", out.SelectedModel)
}

func TestScreenshotChatModelsAreRoutingCandidates(t *testing.T) {
	p := newPlugin(t)
	outputs := []*semanticrouter.PreviewResult{
		preview(t, p, "Hello, how are you today?"),
		preview(t, p, "Answer briefly in one sentence: what is HTTP?"),
		preview(t, p, "Write Python code to debug this API."),
		preview(t, p, "Prove the theorem step by step."),
		preview(t, p, []any{
			map[string]any{"type": "text", "text": "Describe this image."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:inline"}},
		}),
		preview(t, p, []any{
			map[string]any{"type": "text", "text": "Describe this video."},
			map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}},
		}),
	}

	candidates := make(map[string]bool)
	for _, out := range outputs {
		for _, model := range out.Candidates {
			candidates[model] = true
		}
	}
	for _, model := range []string{
		"nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4",
		"nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4",
		"fakellm-openai/gpt-4o",
		"nvidia/nemotron-ultra",
		"gemini/gemini-3.5-flash-lite",
		"anthropic/claude-3.5-sonnet",
		"fakellm-qwen/qwen2.5-72b-instruct",
		"qwen/qwen2.5-vl-72b-instruct",
		"meta/llama-3.1-70b-instruct",
		"mistral/mistral-small-3",
		"meta/llama-3.1-70b-instruct-int4",
		"deepseek/deepseek-r1-distill-32b",
		"google/gemma-2-27b",
		"microsoft/phi-4-14b",
		"meta/llama-3.1-8b-instruct",
	} {
		assert.Truef(t, candidates[model], "model %q is not reachable from any chat decision", model)
	}
	assert.False(t, candidates["aifactory/factory-embed-3"], "embedding-only model must not enter chat routing")
}

// TestDecisionLeadersAreDistinct guards the recipe ordering invariant: no model
// may sit first in more than one decision, otherwise a single card absorbs most
// traffic and the remaining cards never get exercised.
func TestDecisionLeadersAreDistinct(t *testing.T) {
	p := newPlugin(t)
	longInput := make([]byte, 6200)
	for i := range longInput {
		longInput[i] = 'a'
	}

	leaders := map[string]string{}
	for _, tc := range []struct {
		decision string
		content  any
	}{
		{"general", "Hello, how are you today?"},
		{"simple", "Answer briefly in one sentence: what is HTTP?"},
		{"code", "Write a Python function that reverses a linked list."},
		{"deep-reasoning", "Prove the theorem and derive the architecture trade-offs step by step."},
		{"long-context", string(longInput) + " please help"},
		{"tools", "Use the agent loop to call the weather tool and summarize it."},
		{"vision", []any{
			map[string]any{"type": "text", "text": "Describe this image."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:inline"}},
		}},
		{"video", []any{
			map[string]any{"type": "text", "text": "Describe this video."},
			map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}},
		}},
	} {
		out := preview(t, p, tc.content)
		require.Equal(t, tc.decision, out.Decision)
		require.NotEmpty(t, out.Candidates)

		leader := out.Candidates[0]
		previous, taken := leaders[leader]
		assert.Falsef(t, taken, "model %q leads both %q and %q", leader, previous, tc.decision)
		leaders[leader] = tc.decision
	}
}

func TestInitMissingRecipe(t *testing.T) {
	_, err := semanticrouter.Init(&semanticrouter.Config{RecipeFile: ""}, nil)
	require.Error(t, err)
}
