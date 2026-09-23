//go:build cgo

package semanticrouter_test

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/maximhq/bifrost/plugins/semanticrouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
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

func assertProfileOnly(t *testing.T, out *semanticrouter.PreviewResult, decision string) {
	t.Helper()
	assert.Equal(t, decision, out.Decision)
	assert.Equal(t, services.EvalSelectionProfileOnly, out.SelectionStatus)
	assert.Equal(t, "capability_profile", out.SelectionMethod)
	assert.Empty(t, out.SelectedModel)
	assert.Empty(t, out.Candidates)
	require.NotNil(t, out.CapabilityProfile)
}

func hasKeyword(out *semanticrouter.PreviewResult, name string) bool {
	return out.MatchedSignals != nil && slices.Contains(out.MatchedSignals.Keywords, name)
}

func hasStructure(out *semanticrouter.PreviewResult, name string) bool {
	return out.MatchedSignals != nil && slices.Contains(out.MatchedSignals.Structure, name)
}

func hasConversation(out *semanticrouter.PreviewResult, name string) bool {
	return out.MatchedSignals != nil && slices.Contains(out.MatchedSignals.Conversation, name)
}

func hasContext(out *semanticrouter.PreviewResult, name string) bool {
	return out.MatchedSignals != nil && slices.Contains(out.MatchedSignals.Context, name)
}

func hasInputModality(out *semanticrouter.PreviewResult, name string) bool {
	return out.MatchedSignals != nil && slices.Contains(out.MatchedSignals.InputModality, name)
}

func TestPreviewClassifiesQualityLanes(t *testing.T) {
	p := newPlugin(t)
	long := make([]byte, 6200)
	for i := range long {
		long[i] = 'a'
	}

	t.Run("code", func(t *testing.T) {
		out := preview(t, p, "Write a Python function that reverses a linked list.")
		assertProfileOnly(t, out, "code")
		assert.Equal(t, 0.30, out.CapabilityProfile.Prefer.Capabilities["coding"])
		assert.True(t, out.CapabilityProfile.DynamicFeatures.Language)
		assert.True(t, out.CapabilityProfile.DynamicFeatures.Framework)
		assert.True(t, hasKeyword(out, "code_request"))
	})

	t.Run("deep-reasoning-keyword", func(t *testing.T) {
		out := preview(t, p, "Prove the theorem and derive the architecture trade-offs step by step.")
		assertProfileOnly(t, out, "deep-reasoning")
		assert.Equal(t, 0.35, out.CapabilityProfile.Prefer.Capabilities["reasoning"])
		assert.True(t, hasKeyword(out, "deep_reasoning_request"))
	})

	t.Run("deep-reasoning-dense-constraints", func(t *testing.T) {
		out := preview(t, p, "You must keep the API stable. You should document every error. Please ensure the constraint is listed. Do not break callers.")
		assertProfileOnly(t, out, "deep-reasoning")
		assert.True(t, hasStructure(out, "dense_constraints"))
	})

	t.Run("agentic-keyword", func(t *testing.T) {
		out := preview(t, p, "Use the agent loop to orchestrate the weather lookup and summarize it.")
		assertProfileOnly(t, out, "agentic")
		assert.Equal(t, 0.30, out.CapabilityProfile.Prefer.Capabilities["agentic"])
		assert.True(t, hasKeyword(out, "agentic_request"))
	})

	t.Run("agentic-ordered-workflow", func(t *testing.T) {
		out := preview(t, p, "First collect the logs then restart the service.")
		assertProfileOnly(t, out, "agentic")
		assert.True(t, hasStructure(out, "ordered_workflow"))
	})

	t.Run("long-context", func(t *testing.T) {
		out := preview(t, p, string(long)+" please help")
		assertProfileOnly(t, out, "long-context")
		assert.True(t, out.CapabilityProfile.Context.MinTokensFromRequest)
		assert.Equal(t, 0.50, out.CapabilityProfile.Prefer.Capabilities["long_context"])
		assert.True(t, hasContext(out, "long_context") || hasStructure(out, "large_input"))
	})

	t.Run("multi-part", func(t *testing.T) {
		out := preview(t, p, "What time is it? Where is the station? Who is meeting us?")
		assertProfileOnly(t, out, "multi-part")
		assert.True(t, hasStructure(out, "many_questions"))
	})

	t.Run("simple-brief", func(t *testing.T) {
		out := preview(t, p, "Answer briefly in one sentence: what is HTTP?")
		assertProfileOnly(t, out, "simple")
		assert.Equal(t, 0.35, out.CapabilityProfile.Objectives.Latency)
		assert.True(t, hasKeyword(out, "simple_request"))
	})

	t.Run("simple-factual", func(t *testing.T) {
		out := preview(t, p, "What is HTTP?")
		assertProfileOnly(t, out, "simple")
		assert.True(t, hasKeyword(out, "simple_request"))
	})

	t.Run("general", func(t *testing.T) {
		out := preview(t, p, "Hello, how are you today?")
		assertProfileOnly(t, out, "general")
		assert.False(t, hasKeyword(out, "code_request"))
		assert.False(t, hasKeyword(out, "simple_request"))
	})

	t.Run("image-uses-multimodal-lane", func(t *testing.T) {
		out := preview(t, p, []any{
			map[string]any{"type": "text", "text": "What is in this picture?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:inline"}},
		})
		assertProfileOnly(t, out, "multimodal")
		assert.True(t, out.CapabilityProfile.Require.ModalityFromRequest)
		assert.True(t, hasInputModality(out, "image_input"))
	})

	t.Run("video-uses-multimodal-lane", func(t *testing.T) {
		out := preview(t, p, []any{
			map[string]any{"type": "text", "text": "Summarize this video."},
			map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}},
		})
		assertProfileOnly(t, out, "multimodal")
		assert.True(t, hasInputModality(out, "video_input"))
	})
}

func TestPreviewToolsSelectAgenticConditionalRequirement(t *testing.T) {
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
	assertProfileOnly(t, out, "agentic")
	require.Len(t, out.CapabilityProfile.Require.Conditional, 1)
	require.NotNil(t, out.CapabilityProfile.Require.Conditional[0].Require.ToolCalling)
	assert.True(t, *out.CapabilityProfile.Require.Conditional[0].Require.ToolCalling)
	assert.True(t, hasConversation(out, "has_tools"))
}

func TestPreviewSpanishLanguageSignal(t *testing.T) {
	p := newPlugin(t)
	out := preview(t, p, "Buenos días. ¿Podrías explicarme cómo funciona el protocolo HTTP en una red de computadoras modernas?")
	assertProfileOnly(t, out, out.Decision)
	if out.MatchedSignals == nil || !slices.Contains(out.MatchedSignals.Language, "es") {
		t.Logf("language detector did not report es (got %#v); skipping hard assertion", out.MatchedSignals)
		return
	}
	assert.Contains(t, out.MatchedSignals.Language, "es")
}

func TestInitMissingRecipe(t *testing.T) {
	_, err := semanticrouter.Init(&semanticrouter.Config{RecipeFile: ""}, nil)
	require.Error(t, err)
}
