package config

import (
	"fmt"
	"math"
	"strings"
)

const (
	CapabilityReasoning            = "reasoning"
	CapabilityAnalysis             = "analysis"
	CapabilityMathematics          = "mathematics"
	CapabilityArchitecture         = "architecture"
	CapabilityCoding               = "coding"
	CapabilityCodeGeneration       = "code_generation"
	CapabilityDebugging            = "debugging"
	CapabilityCodeReview           = "code_review"
	CapabilitySoftwareEngineering  = "software_engineering"
	CapabilityAgentic              = "agentic"
	CapabilityPlanning             = "planning"
	CapabilityToolUse              = "tool_use"
	CapabilityInstructionFollowing = "instruction_following"
	CapabilityLongContext          = "long_context"
	CapabilityContextUnderstanding = "context_understanding"
	CapabilityMultiTask            = "multi_task"
	CapabilitySummarisation        = "summarisation"
	CapabilityGeneralKnowledge     = "general_knowledge"
	CapabilityFactualQA            = "factual_qa"
	CapabilityLowComplexity        = "low_complexity"
	CapabilityVisualReasoning      = "visual_reasoning"
	CapabilityMultimodalReasoning  = "multimodal_reasoning"
	CapabilityImageUnderstanding   = "image_understanding"
	CapabilityVideoUnderstanding   = "video_understanding"
	CapabilityConversation         = "conversation"
)

var allowedCapabilityNames = map[string]struct{}{
	CapabilityReasoning:            {},
	CapabilityAnalysis:             {},
	CapabilityMathematics:          {},
	CapabilityArchitecture:         {},
	CapabilityCoding:               {},
	CapabilityCodeGeneration:       {},
	CapabilityDebugging:            {},
	CapabilityCodeReview:           {},
	CapabilitySoftwareEngineering:  {},
	CapabilityAgentic:              {},
	CapabilityPlanning:             {},
	CapabilityToolUse:              {},
	CapabilityInstructionFollowing: {},
	CapabilityLongContext:          {},
	CapabilityContextUnderstanding: {},
	CapabilityMultiTask:            {},
	CapabilitySummarisation:        {},
	CapabilityGeneralKnowledge:     {},
	CapabilityFactualQA:            {},
	CapabilityLowComplexity:        {},
	CapabilityVisualReasoning:      {},
	CapabilityMultimodalReasoning:  {},
	CapabilityImageUnderstanding:   {},
	CapabilityVideoUnderstanding:   {},
	CapabilityConversation:         {},
}

var allowedInputModalities = map[string]struct{}{
	"text":  {},
	"image": {},
	"audio": {},
	"video": {},
}

// CapabilityProfile is the inventory-independent quality contract a decision
// declares. Preview returns it so a caller (Bifrost governance) can filter a
// live catalog. It is not a model pin list.
type CapabilityProfile struct {
	Require         CapabilityRequirements `yaml:"require,omitempty" json:"require,omitempty"`
	Prefer          CapabilityPreferences  `yaml:"prefer,omitempty" json:"prefer,omitempty"`
	DynamicFeatures DynamicFeatureProfile  `yaml:"dynamic_features,omitempty" json:"dynamic_features,omitempty"`
	Context         CapabilityContext      `yaml:"context,omitempty" json:"context,omitempty"`
	Objectives      CapabilityObjectives   `yaml:"objectives,omitempty" json:"objectives,omitempty"`
}

type CapabilityRequirements struct {
	InputModalities     []string                           `yaml:"input_modalities,omitempty" json:"input_modalities,omitempty"`
	ModalityFromRequest bool                               `yaml:"modality_from_request,omitempty" json:"modality_from_request,omitempty"`
	Conditional         []ConditionalCapabilityRequirement `yaml:"conditional,omitempty" json:"conditional,omitempty"`
}

type ConditionalCapabilityRequirement struct {
	When    CapabilityCondition     `yaml:"when" json:"when"`
	Require ConditionalRequirements `yaml:"require" json:"require"`
}

type CapabilityCondition struct {
	Type string `yaml:"type" json:"type"`
	Name string `yaml:"name" json:"name"`
}

type ConditionalRequirements struct {
	ToolCalling *bool `yaml:"tool_calling,omitempty" json:"tool_calling,omitempty"`
}

type CapabilityPreferences struct {
	Capabilities map[string]float64 `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

type DynamicFeatureProfile struct {
	Language  bool `yaml:"language,omitempty" json:"language,omitempty"`
	Framework bool `yaml:"framework,omitempty" json:"framework,omitempty"`
}

type CapabilityContext struct {
	MinTokensFromRequest bool `yaml:"min_tokens_from_request,omitempty" json:"min_tokens_from_request,omitempty"`
}

type CapabilityObjectives struct {
	CapabilityFit float64 `yaml:"capability_fit,omitempty" json:"capability_fit,omitempty"`
	Quality       float64 `yaml:"quality,omitempty" json:"quality,omitempty"`
	Latency       float64 `yaml:"latency,omitempty" json:"latency,omitempty"`
	Cost          float64 `yaml:"cost,omitempty" json:"cost,omitempty"`
}

func (p *CapabilityProfile) Clone() *CapabilityProfile {
	if p == nil {
		return nil
	}
	out := *p
	out.Require.InputModalities = append([]string(nil), p.Require.InputModalities...)
	out.Require.Conditional = append([]ConditionalCapabilityRequirement(nil), p.Require.Conditional...)
	if p.Prefer.Capabilities != nil {
		out.Prefer.Capabilities = make(map[string]float64, len(p.Prefer.Capabilities))
		for capability, weight := range p.Prefer.Capabilities {
			out.Prefer.Capabilities[capability] = weight
		}
	}
	return &out
}

func validateCapabilityProfile(decisionName string, profile *CapabilityProfile) error {
	if profile == nil {
		return nil
	}
	if err := validateInputModalities(decisionName, profile.Require.InputModalities); err != nil {
		return err
	}
	if err := validateConditionalRequirements(decisionName, profile.Require.Conditional); err != nil {
		return err
	}
	if err := validateCapabilityWeights(decisionName, profile.Prefer.Capabilities); err != nil {
		return err
	}
	if err := validateObjectives(decisionName, profile.Objectives); err != nil {
		return err
	}
	return nil
}

func validateInputModalities(decisionName string, values []string) error {
	if len(values) > 16 {
		return fmt.Errorf("decision '%s': capability_profile.require.input_modalities cannot contain more than 16 entries", decisionName)
	}
	seen := make(map[string]struct{}, len(values))
	for i, raw := range values {
		name := strings.TrimSpace(strings.ToLower(raw))
		if name == "" {
			return fmt.Errorf("decision '%s': capability_profile.require.input_modalities[%d] cannot be empty", decisionName, i)
		}
		if _, ok := allowedInputModalities[name]; !ok {
			return fmt.Errorf("decision '%s': capability_profile.require.input_modalities %q is not supported", decisionName, raw)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("decision '%s': capability_profile.require.input_modalities duplicates %q", decisionName, name)
		}
		seen[name] = struct{}{}
		values[i] = name
	}
	return nil
}

func validateConditionalRequirements(decisionName string, conditions []ConditionalCapabilityRequirement) error {
	for i := range conditions {
		condition := &conditions[i]
		condition.When.Type = strings.TrimSpace(strings.ToLower(condition.When.Type))
		condition.When.Name = strings.TrimSpace(condition.When.Name)
		if condition.When.Type == "" || condition.When.Name == "" {
			return fmt.Errorf("decision '%s': capability_profile.require.conditional[%d].when requires type and name", decisionName, i)
		}
		if condition.Require.ToolCalling == nil {
			return fmt.Errorf("decision '%s': capability_profile.require.conditional[%d].require must declare tool_calling", decisionName, i)
		}
		if !*condition.Require.ToolCalling {
			return fmt.Errorf("decision '%s': capability_profile.require.conditional[%d].require.tool_calling must be true", decisionName, i)
		}
	}
	return nil
}

func validateCapabilityWeights(decisionName string, weights map[string]float64) error {
	if len(weights) > 32 {
		return fmt.Errorf("decision '%s': capability_profile.prefer.capabilities cannot contain more than 32 entries", decisionName)
	}
	for rawName, weight := range weights {
		name := strings.TrimSpace(strings.ToLower(rawName))
		if _, ok := allowedCapabilityNames[name]; !ok {
			return fmt.Errorf("decision '%s': capability_profile.prefer.capabilities %q is not supported", decisionName, rawName)
		}
		if name != rawName {
			delete(weights, rawName)
			weights[name] = weight
		}
		if weight <= 0 || weight > 1 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return fmt.Errorf("decision '%s': capability_profile.prefer.capabilities.%s must be in (0,1]", decisionName, name)
		}
	}
	return nil
}

func validateObjectives(decisionName string, objectives CapabilityObjectives) error {
	values := map[string]float64{
		"capability_fit": objectives.CapabilityFit,
		"quality":        objectives.Quality,
		"latency":        objectives.Latency,
		"cost":           objectives.Cost,
	}
	total := 0.0
	for name, value := range values {
		if value < 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("decision '%s': capability_profile.objectives.%s must be in [0,1]", decisionName, name)
		}
		total += value
	}
	if math.Abs(total-1) > 0.000001 {
		return fmt.Errorf("decision '%s': capability_profile.objectives weights must sum to 1, got %.6f", decisionName, total)
	}
	return nil
}

func validateDecisionCapabilityProfile(decision Decision) error {
	if err := validateCapabilityProfile(decision.Name, decision.CapabilityProfile); err != nil {
		return err
	}
	if decision.CapabilityProfile == nil {
		return nil
	}
	if len(decision.ModelRefs) > 0 {
		return fmt.Errorf("decision '%s': capability_profile cannot be combined with modelRefs", decision.Name)
	}
	if len(decision.CandidateIterations) > 0 {
		return fmt.Errorf("decision '%s': capability_profile cannot be combined with candidateIterations", decision.Name)
	}
	if decision.Algorithm == nil {
		return nil
	}
	algorithmType := strings.TrimSpace(decision.Algorithm.Type)
	if IsLooperAlgorithmType(algorithmType) || strings.EqualFold(algorithmType, DecisionAlgorithmPrompt) {
		return fmt.Errorf("decision '%s': capability_profile cannot use algorithm type %q", decision.Name, algorithmType)
	}
	return nil
}
