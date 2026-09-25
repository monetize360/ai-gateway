package governance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadModelOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	raw := `{
  "models": {
    "gemini/gemini-2.5-pro": {
      "display_name": "Gemini 2.5 Pro",
      "vendor": "Google",
      "category": "Reasoning, flagship",
      "context_window": "Up to 1M tokens",
      "capability_tier": "flagship",
      "supports_reasoning": true,
      "status": "live",
      "categories": ["Code Generation"]
    },
    "gemini/gemini-2.5-flash-lite": {
      "capability_tier": "small",
      "supports_reasoning": false
    }
  }
}`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

	overrides, err := loadModelOverridesFile(path)
	require.NoError(t, err)
	require.Len(t, overrides, 2)

	pro, ok := overrides["gemini/gemini-2.5-pro"]
	require.True(t, ok)
	assert.Equal(t, "flagship", pro.CapabilityTier)
	require.NotNil(t, pro.SupportsReasoning)
	assert.True(t, *pro.SupportsReasoning)
	assert.Equal(t, "live", pro.Status)
	require.NotNil(t, pro.Sheet)
	assert.Equal(t, "Gemini 2.5 Pro", pro.Sheet.DisplayName)
	assert.Equal(t, "Google", pro.Sheet.Vendor)
	assert.Equal(t, "Reasoning, flagship", pro.Sheet.Category)
	assert.Equal(t, "Up to 1M tokens", pro.Sheet.ContextWindow)
	assert.Contains(t, pro.Categories, "Code Generation")

	flashLite, ok := overrides["gemini/gemini-2.5-flash-lite"]
	require.True(t, ok)
	assert.Equal(t, "small", flashLite.CapabilityTier)
	require.NotNil(t, flashLite.SupportsReasoning)
	assert.False(t, *flashLite.SupportsReasoning)
}

func TestMergeModelOverridesInlineWins(t *testing.T) {
	file := map[string]modelcatalog.ModelRoutingOverride{
		"gemini/gemini-2.5-pro": {CapabilityTier: "flagship"},
	}
	inline := map[string]modelcatalog.ModelRoutingOverride{
		"gemini/gemini-2.5-pro": {CapabilityTier: "large"},
	}
	merged := mergeModelOverrides(file, inline)
	assert.Equal(t, "large", merged["gemini/gemini-2.5-pro"].CapabilityTier)
}
