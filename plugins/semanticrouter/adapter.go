//go:build cgo

package semanticrouter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

func toIntentRequest(input PreviewInput, defaultEntrypoint string) (services.IntentRequest, error) {
	request := services.IntentRequest{
		Model:    strings.TrimSpace(input.Model),
		Text:     input.Text,
		Metadata: input.Metadata,
	}
	if request.Model == "" {
		request.Model = defaultEntrypoint
	}

	if len(input.Messages) > 0 {
		data, err := json.Marshal(input.Messages)
		if err != nil {
			return services.IntentRequest{}, fmt.Errorf("marshal preview messages: %w", err)
		}
		if err := json.Unmarshal(data, &request.Messages); err != nil {
			return services.IntentRequest{}, fmt.Errorf("map preview messages: %w", err)
		}
	}

	var err error
	if request.Tools, err = rawList(input.Tools); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview tools: %w", err)
	}
	if request.Functions, err = rawList(input.Functions); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview functions: %w", err)
	}
	if request.ToolChoice, err = rawValue(input.ToolChoice); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview tool_choice: %w", err)
	}
	if request.FunctionCall, err = rawValue(input.FunctionCall); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview function_call: %w", err)
	}
	if request.ResponseFormat, err = rawValue(input.ResponseFormat); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview response_format: %w", err)
	}
	if request.MaxTokens, err = rawValue(input.MaxTokens); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview max_tokens: %w", err)
	}
	if request.MaxCompletionTokens, err = rawValue(input.MaxCompletionTokens); err != nil {
		return services.IntentRequest{}, fmt.Errorf("map preview max_completion_tokens: %w", err)
	}
	if len(input.PreviewContext) > 0 {
		data, marshalErr := json.Marshal(input.PreviewContext)
		if marshalErr != nil {
			return services.IntentRequest{}, fmt.Errorf("marshal preview context: %w", marshalErr)
		}
		var previewContext services.PreviewContext
		if unmarshalErr := json.Unmarshal(data, &previewContext); unmarshalErr != nil {
			return services.IntentRequest{}, fmt.Errorf("map preview context: %w", unmarshalErr)
		}
		request.PreviewContext = &previewContext
	}
	return request, nil
}

func fromEvalResponse(response *services.EvalResponse) *PreviewResult {
	if response == nil {
		return nil
	}
	result := &PreviewResult{
		SelectedModel:   response.SelectedModel,
		SelectionStatus: response.SelectionStatus,
		SelectionMethod: response.SelectionMethod,
		Candidates:      append([]string(nil), response.RecommendedModels...),
		MatchedSignals:  cloneMatchedSignals(response),
	}
	if response.CapabilityProfile != nil {
		result.CapabilityProfile = response.CapabilityProfile.Clone()
	}
	if response.DecisionResult != nil {
		result.Decision = response.DecisionResult.DecisionName
		result.Algorithm = response.DecisionResult.Algorithm
	}
	if result.Decision == "" {
		result.Decision = response.RoutingDecision
	}
	if result.Algorithm == "" {
		result.Algorithm = response.SelectionMethod
	}
	return result
}

func cloneMatchedSignals(response *services.EvalResponse) *services.MatchedSignals {
	if response == nil {
		return nil
	}
	if response.DecisionResult != nil && response.DecisionResult.MatchedSignals != nil {
		copy := *response.DecisionResult.MatchedSignals
		return &copy
	}
	return nil
}

func rawValue(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func rawList(value any) ([]json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}
