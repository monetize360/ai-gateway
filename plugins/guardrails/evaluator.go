package guardrails

import (
	"context"
	"sort"
	"time"
)

const (
	DecisionAllow  = "allow"
	DecisionBlock  = "block"
	DecisionRedact = "redact"
	DecisionFlag   = "flag"
)

// EvaluationInput is the text a single policy evaluates.
type EvaluationInput struct {
	Stage    string
	Messages []MessageText
}

// Evaluation is what an evaluator returns before the policy action is applied.
// Replacement, when set, is the full redacted text used for a redact action.
type Evaluation struct {
	Matched     bool
	Reasons     []string
	Replacement string
}

// SettingField describes one config_json property the catalog form can render.
type SettingField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     any      `json:"default,omitempty"`
}

// TypeInfo is one entry in the guardrail type catalog.
type TypeInfo struct {
	Type         string         `json:"type"`
	Description  string         `json:"description"`
	Stages       []string       `json:"stages"`
	Settings     []SettingField `json:"settings"`
	ConfigSchema map[string]any `json:"config_schema"`
}

// Verdict is the policy action applied to one evaluation.
type Verdict struct {
	Decision   string
	PolicyID   string
	PolicyName string
	Reasons    []string
	Latency    time.Duration
	Messages   []MessageText
}

// GuardrailEvaluator checks one policy type.
type GuardrailEvaluator interface {
	Type() string
	Info() TypeInfo
	Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error)
}

// EvaluatorRegistry looks up evaluators by logical type name.
type EvaluatorRegistry struct {
	byType map[string]GuardrailEvaluator
}

func NewEvaluatorRegistry(evaluators ...GuardrailEvaluator) *EvaluatorRegistry {
	registry := &EvaluatorRegistry{byType: map[string]GuardrailEvaluator{}}
	for _, evaluator := range evaluators {
		if evaluator == nil {
			continue
		}
		registry.byType[evaluator.Type()] = evaluator
	}
	return registry
}

func (r *EvaluatorRegistry) Get(typeName string) (GuardrailEvaluator, bool) {
	if r == nil {
		return nil, false
	}
	evaluator, ok := r.byType[typeName]
	return evaluator, ok
}

// List returns the catalog in stable type-name order.
func (r *EvaluatorRegistry) List() []TypeInfo {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.byType))
	for name := range r.byType {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]TypeInfo, 0, len(names))
	for _, name := range names {
		out = append(out, r.byType[name].Info())
	}
	return out
}
