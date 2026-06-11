package tables

import (
	"fmt"
	"strings"
)

// AI Gateway provider picklist category (MPilot picklists/finops/ai_gateway_provider.json).
const AIGatewayProviderPicklistCategoryID = "3cfb7d8a-d898-487c-b786-ff5f0e76bd50"

// CustomProviderPicklistItemID is the picklist item for custom (non-standard) providers.
const CustomProviderPicklistItemID = "2382e8b8-bfb0-4c6d-a273-2e5b55fa13a8"

// providerPicklistIDToName maps MPilot picklist item IDs to Bifrost ModelProvider names.
// Keep in sync with picklists/finops/ai_gateway_provider.json.
var providerPicklistIDToName = map[string]string{
	"94e96a44-740d-4372-a0e0-bc9837173c3a": "anthropic",
	"63a9d811-67c3-435d-8ab9-7eb330313f77": "azure",
	"498ad9a1-bd88-4d5e-a94c-23a82a89c7d9": "bedrock",
	"3c6fc643-f00c-438c-868e-024464e99e56": "cerebras",
	"a09093f1-096f-4d4a-b95b-6a5355b2215d": "cohere",
	"d7c998e8-178e-4d84-a114-8a95460aec2a": "gemini",
	"f51459ec-5b3b-442d-aa9d-7a0d1bbfa929": "groq",
	"1a18fe88-ea83-49a1-b70d-7671c17ec6ef": "mistral",
	"c38e9cbb-9c1c-4e28-ab75-32bbf7b48c6b": "ollama",
	"145e3a40-2018-4f73-a51e-ffb63a79e861": "openai",
	"2d949387-1087-48c8-9782-de4e4914744f": "parasail",
	"55ba5daa-dd04-4bca-a85b-258389f18378": "perplexity",
	"69e5752d-8707-4470-9b7b-826062f76335": "sgl",
	"e71db6cb-0c72-4e19-9c7d-f3fd8daa2555": "vertex",
	"5568452c-11ba-49f2-af3f-98bf4a52b045": "openrouter",
	"60aa81fc-2133-41a0-9889-ae7a8586d1a7": "elevenlabs",
	"8ad88d7b-42e4-4618-b13e-71bc0235ad08": "huggingface",
	"4885dbb8-f309-43fd-bb61-e5e87975c66d": "nebius",
	"4d664ca9-e5be-40d4-b5f0-7a3a76a5ea47": "xai",
	"fff3f397-2a3a-436a-bb43-494b6b501599": "replicate",
	"e710614b-d2cf-446e-9d90-4344208426d8": "vllm",
	"66c23df2-e276-47d3-8f93-f44c3536f3f0": "runway",
	"b702493d-1741-45da-8111-3f2ebab753bb": "fireworks",
}

var providerNameToPicklistID = func() map[string]string {
	out := make(map[string]string, len(providerPicklistIDToName))
	for id, name := range providerPicklistIDToName {
		out[name] = id
	}
	return out
}()

// IsCustomProviderPicklistItem reports whether picklistItemID is the custom-provider type.
func IsCustomProviderPicklistItem(picklistItemID string) bool {
	return strings.TrimSpace(picklistItemID) == CustomProviderPicklistItemID
}

// ProviderNameFromPicklistItem returns the Bifrost provider name for a standard picklist item ID.
func ProviderNameFromPicklistItem(picklistItemID string) (string, bool) {
	name, ok := providerPicklistIDToName[strings.TrimSpace(picklistItemID)]
	return name, ok
}

// PicklistItemIDForProviderName returns the picklist item ID for a standard provider name.
func PicklistItemIDForProviderName(providerName string) (string, bool) {
	id, ok := providerNameToPicklistID[strings.TrimSpace(providerName)]
	return id, ok
}

func providerHasCustomConfig(p *TableProvider) bool {
	if p == nil {
		return false
	}
	if p.CustomProviderConfig != nil {
		return true
	}
	return strings.TrimSpace(p.CustomProviderConfigJSON) != "" && strings.TrimSpace(p.CustomProviderConfigJSON) != "{}"
}

// RuntimeProviderKey returns the ModelProvider string used by the inference engine.
// Standard providers are resolved from provider_type; custom providers use name.
func (p *TableProvider) RuntimeProviderKey() (string, error) {
	if p == nil {
		return "", fmt.Errorf("provider is nil")
	}
	if err := p.SyncProviderTypeAssociations(); err != nil {
		return "", err
	}
	if !isNonEmptyString(p.ProviderType) {
		if strings.TrimSpace(p.Name) == "" {
			return "", fmt.Errorf("provider has no provider_type or name")
		}
		return strings.TrimSpace(p.Name), nil
	}
	typeID := strings.TrimSpace(*p.ProviderType)
	if IsCustomProviderPicklistItem(typeID) {
		if strings.TrimSpace(p.Name) == "" {
			return "", fmt.Errorf("custom provider requires name")
		}
		return strings.TrimSpace(p.Name), nil
	}
	name, ok := ProviderNameFromPicklistItem(typeID)
	if !ok {
		return "", fmt.Errorf("unknown provider_type %q", typeID)
	}
	return name, nil
}

// SyncProviderTypeAssociations keeps provider_type and name aligned.
// Standard providers are selected by provider_type; name is derived from the picklist item.
// Custom providers use provider_type=custom and keep name as the custom provider key.
func (p *TableProvider) SyncProviderTypeAssociations() error {
	if p == nil {
		return nil
	}

	if !isNonEmptyString(p.ProviderType) {
		if providerHasCustomConfig(p) {
			p.ProviderType = stringPtr(CustomProviderPicklistItemID)
		} else if id, ok := PicklistItemIDForProviderName(p.Name); ok {
			p.ProviderType = stringPtr(id)
		}
	}

	if !isNonEmptyString(p.ProviderType) {
		return nil
	}

	typeID := strings.TrimSpace(*p.ProviderType)
	if IsCustomProviderPicklistItem(typeID) {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("name is required for custom providers")
		}
		if _, ok := PicklistItemIDForProviderName(p.Name); ok {
			return fmt.Errorf("custom provider name %q cannot match a standard provider", p.Name)
		}
		return nil
	}

	providerName, ok := ProviderNameFromPicklistItem(typeID)
	if !ok {
		return fmt.Errorf("unknown provider_type %q", typeID)
	}
	p.Name = providerName
	return nil
}

func stringPtr(s string) *string {
	return &s
}
