//go:build cgo

package semanticrouter

import (
	"encoding/json"
	"testing"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

func TestToIntentRequestPreservesRoutingFacts(t *testing.T) {
	request, err := toIntentRequest(PreviewInput{
		Messages: []map[string]any{{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "inspect"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:inline"}},
			},
		}},
		Tools:          []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		ToolChoice:     "auto",
		ResponseFormat: map[string]any{"type": "json_object"},
		MaxTokens:      256,
		Metadata:       map[string]string{"tenant": "test"},
		PreviewContext: map[string]any{"session_id": "session-1"},
	}, "vllm-sr/auto")
	if err != nil {
		t.Fatalf("toIntentRequest() error = %v", err)
	}
	if request.Model != "vllm-sr/auto" || len(request.Messages) != 1 || len(request.Tools) != 1 {
		t.Fatalf("mapped request = %#v", request)
	}
	if string(request.ToolChoice) != `"auto"` || string(request.MaxTokens) != "256" {
		t.Fatalf("tool_choice=%s max_tokens=%s", request.ToolChoice, request.MaxTokens)
	}
	if request.PreviewContext == nil || request.PreviewContext.SessionID != "session-1" {
		t.Fatalf("preview_context = %#v", request.PreviewContext)
	}
	var content []map[string]any
	if err := json.Unmarshal(request.Messages[0].Content, &content); err != nil || len(content) != 2 {
		t.Fatalf("message content = %s err=%v", request.Messages[0].Content, err)
	}
}

func TestFromEvalResponseMapsStableGovernanceContract(t *testing.T) {
	response := &services.EvalResponse{
		DecisionResult: &services.EvalDecisionResult{
			DecisionName: "code-route",
			Algorithm:    config.DecisionAlgorithmMultiFactor,
			MatchedSignals: &services.MatchedSignals{
				Keywords: []string{"code_request"},
			},
		},
		RecommendedModels: []string{"model-a", "model-b"},
		SelectedModel:     "model-b",
		SelectionStatus:   services.EvalSelectionSelected,
		SelectionMethod:   "multi_factor",
		CapabilityProfile: &config.CapabilityProfile{
			Prefer: config.CapabilityPreferences{Capabilities: map[string]float64{
				config.CapabilityReasoning: 1,
			}},
			Objectives: config.CapabilityObjectives{CapabilityFit: 1},
		},
	}
	result := fromEvalResponse(response)
	if result.SelectedModel != "model-b" || result.Decision != "code-route" ||
		result.Algorithm != "multi_factor" || len(result.Candidates) != 2 {
		t.Fatalf("PreviewResult = %#v", result)
	}
	if result.CapabilityProfile == nil || result.CapabilityProfile.Prefer.Capabilities[config.CapabilityReasoning] != 1 {
		t.Fatalf("CapabilityProfile = %#v", result.CapabilityProfile)
	}
	if result.MatchedSignals == nil || len(result.MatchedSignals.Keywords) != 1 {
		t.Fatalf("MatchedSignals = %#v", result.MatchedSignals)
	}
}
