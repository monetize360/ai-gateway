package governance

import (
	"fmt"
	"os"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

// pocModelRoutingFile is the on-disk shape for optional model_overrides_file catalogs.
type pocModelRoutingFile struct {
	Description string                          `json:"description,omitempty"`
	Models      map[string]pocModelRoutingEntry `json:"models"`
}

type pocModelRoutingEntry struct {
	ModelID             string   `json:"model_id,omitempty"`
	DisplayName         string   `json:"display_name,omitempty"`
	Vendor              string   `json:"vendor,omitempty"`
	Registry            string   `json:"registry,omitempty"`
	ServingMode         string   `json:"serving_mode,omitempty"`
	Category            string   `json:"category,omitempty"`
	Parameters          string   `json:"parameters,omitempty"`
	Quantization        string   `json:"quantization,omitempty"`
	ContextWindow       string   `json:"context_window,omitempty"`
	ContextWindowTokens *int     `json:"context_window_tokens,omitempty"`
	Modality            string   `json:"modality,omitempty"`
	Framework           string   `json:"framework,omitempty"`
	GPU                 string   `json:"gpu,omitempty"`
	GPUCount            string   `json:"gpu_count,omitempty"`
	License             string   `json:"license,omitempty"`
	Status              string   `json:"status,omitempty"`
	CapabilityTier      string   `json:"capability_tier,omitempty"`
	SupportsReasoning   *bool    `json:"supports_reasoning,omitempty"`
	LatencyClass        string   `json:"latency_class,omitempty"`
	Categories          []string `json:"categories,omitempty"`
}

// loadModelOverridesFile reads a POC routing JSON and returns ModelRoutingOverride
// entries keyed by provider/model. Inline ModelOverrides in config win on key conflict.
func loadModelOverridesFile(path string) (map[string]modelcatalog.ModelRoutingOverride, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model overrides file %q: %w", path, err)
	}
	var file pocModelRoutingFile
	if err := sonic.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse model overrides file %q: %w", path, err)
	}
	if len(file.Models) == 0 {
		return nil, nil
	}

	out := make(map[string]modelcatalog.ModelRoutingOverride, len(file.Models))
	for key, entry := range file.Models {
		id := strings.TrimSpace(key)
		if entry.ModelID != "" {
			id = strings.TrimSpace(entry.ModelID)
		}
		if id == "" {
			continue
		}
		out[id] = entry.toOverride()
	}
	return out, nil
}

func (e pocModelRoutingEntry) toOverride() modelcatalog.ModelRoutingOverride {
	return modelcatalog.ModelRoutingOverride{
		Categories:        append([]string(nil), e.Categories...),
		CapabilityTier:    e.CapabilityTier,
		SupportsReasoning: e.SupportsReasoning,
		Status:            e.Status,
		LatencyClass:      e.LatencyClass,
		Sheet: &modelcatalog.ModelRoutingSheet{
			DisplayName:   e.DisplayName,
			Vendor:        e.Vendor,
			Registry:      e.Registry,
			ServingMode:   e.ServingMode,
			Category:      e.Category,
			Parameters:    e.Parameters,
			Quantization:  e.Quantization,
			ContextWindow: e.ContextWindow,
			Modality:      e.Modality,
			Framework:     e.Framework,
			GPU:           e.GPU,
			GPUCount:      e.GPUCount,
			License:       e.License,
		},
	}
}

// mergeModelOverrides returns file overrides with inline overrides layered on top
// (inline wins for the same key).
func mergeModelOverrides(fileOverrides, inline map[string]modelcatalog.ModelRoutingOverride) map[string]modelcatalog.ModelRoutingOverride {
	if len(fileOverrides) == 0 && len(inline) == 0 {
		return nil
	}
	out := make(map[string]modelcatalog.ModelRoutingOverride, len(fileOverrides)+len(inline))
	for k, v := range fileOverrides {
		out[k] = v
	}
	for k, v := range inline {
		out[k] = v
	}
	return out
}

func (c *SemanticRoutingConfig) loadModelOverridesFromFile() error {
	if c == nil || strings.TrimSpace(c.ModelOverridesFile) == "" {
		return nil
	}
	fileOverrides, err := loadModelOverridesFile(c.ModelOverridesFile)
	if err != nil {
		return err
	}
	c.ModelOverrides = mergeModelOverrides(fileOverrides, c.ModelOverrides)
	return nil
}
