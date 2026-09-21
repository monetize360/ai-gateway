package modelcatalog

import (
	"strings"

	"github.com/bytedance/sonic"
)

// CapabilityTier ranks model size/capability for complexity-aware routing.
type CapabilityTier string

const (
	CapabilityTierNano     CapabilityTier = "nano"
	CapabilityTierSmall    CapabilityTier = "small"
	CapabilityTierMid      CapabilityTier = "mid"
	CapabilityTierLarge    CapabilityTier = "large"
	CapabilityTierFlagship CapabilityTier = "flagship"
)

// ModelStatus gates whether a model may be selected.
type ModelStatus string

const (
	ModelStatusLive       ModelStatus = "live"
	ModelStatusPreview    ModelStatus = "preview"
	ModelStatusDeprecated ModelStatus = "deprecated"
)

// ModelRoutingSheet holds catalog/ops metadata that is stored but not used by the ranker.
type ModelRoutingSheet struct {
	DisplayName   string `json:"display_name,omitempty"`
	Vendor        string `json:"vendor,omitempty"`
	Registry      string `json:"registry,omitempty"`
	ServingMode   string `json:"serving_mode,omitempty"`
	Category      string `json:"category,omitempty"`
	Parameters    string `json:"parameters,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	ContextWindow string `json:"context_window,omitempty"`
	Modality      string `json:"modality,omitempty"`
	Framework     string `json:"framework,omitempty"`
	GPU           string `json:"gpu,omitempty"`
	GPUCount      string `json:"gpu_count,omitempty"`
	License       string `json:"license,omitempty"`
}

// ModelRoutingOverride is an operator annotation applied on top of datasheet rows.
// Keys must use the same provider/model strings as virtual-key allowlists and as
// vLLM-SR decision modelRefs so Layer 1 ∩ Layer 2 intersection succeeds.
type ModelRoutingOverride struct {
	Categories        []string           `json:"categories,omitempty"`
	CapabilityTier    string             `json:"capability_tier,omitempty"`
	SupportsReasoning *bool              `json:"supports_reasoning,omitempty"`
	Status            string             `json:"status,omitempty"`
	LatencyClass      string             `json:"latency_class,omitempty"`
	Sheet             *ModelRoutingSheet `json:"sheet,omitempty"`
}

// NormalizeCapabilityTier maps free-form tier strings onto the known set.
func NormalizeCapabilityTier(tier string) CapabilityTier {
	switch CapabilityTier(strings.ToLower(strings.TrimSpace(tier))) {
	case CapabilityTierNano:
		return CapabilityTierNano
	case CapabilityTierSmall:
		return CapabilityTierSmall
	case CapabilityTierMid, "medium":
		return CapabilityTierMid
	case CapabilityTierLarge:
		return CapabilityTierLarge
	case CapabilityTierFlagship, "frontier":
		return CapabilityTierFlagship
	default:
		return CapabilityTierMid
	}
}

// ApplyRoutingOverride returns a shallow copy of entry with override fields applied.
// When entry is nil, a minimal entry is created from the override alone.
func ApplyRoutingOverride(entry *PricingEntry, override ModelRoutingOverride) *PricingEntry {
	var out PricingEntry
	if entry != nil {
		out = *entry
	}
	if len(override.Categories) > 0 {
		out.Categories = append([]string(nil), override.Categories...)
	}
	if override.CapabilityTier != "" {
		out.CapabilityTier = NormalizeCapabilityTier(override.CapabilityTier)
	}
	if override.SupportsReasoning != nil {
		out.SupportsReasoning = override.SupportsReasoning
	}
	if override.Status != "" {
		out.Status = ModelStatus(strings.ToLower(strings.TrimSpace(override.Status)))
	}
	if override.LatencyClass != "" {
		out.LatencyClass = override.LatencyClass
	}
	if override.Sheet != nil {
		sheet := *override.Sheet
		out.Sheet = &sheet
	}
	return &out
}

func marshalRoutingSheet(sheet *ModelRoutingSheet) []byte {
	if sheet == nil {
		return nil
	}
	raw, err := sonic.Marshal(sheet)
	if err != nil {
		return nil
	}
	return raw
}

func unmarshalRoutingSheet(raw []byte) *ModelRoutingSheet {
	if len(raw) == 0 {
		return nil
	}
	var sheet ModelRoutingSheet
	if err := sonic.Unmarshal(raw, &sheet); err != nil {
		return nil
	}
	return &sheet
}
