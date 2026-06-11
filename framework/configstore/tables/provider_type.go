package tables

import (
	"fmt"
	"strings"
)

// AI Gateway provider picklist category (MPilot picklists/finops/ai_gateway_provider.json).
const AIGatewayProviderPicklistCategoryID = "7a8b9c0d-1e2f-4a3b-8c9d-0e1f2a3b4c5e"

// CustomProviderPicklistItemID is the picklist item for custom (non-standard) providers.
const CustomProviderPicklistItemID = "11b352c3-d94d-4d71-9118-a307124a2cf2"

// providerPicklistIDToName maps MPilot picklist item IDs to Bifrost ModelProvider names.
// Keep in sync with picklists/finops/ai_gateway_provider.json.
var providerPicklistIDToName = map[string]string{
	"d3fa620b-10f4-4135-93c6-52f22099a5a9": "anthropic",
	"a65a4896-5356-4755-9b7e-1589212d5ae0": "azure",
	"d3bc3107-8341-46ff-9443-80a13912111e": "bedrock",
	"f989621f-c2cc-4fa9-9760-540daa619a22": "cerebras",
	"7e6afda0-51c8-4844-852a-96660fe16eb1": "cohere",
	"92e17c0d-132a-45e4-a961-c20d3cf51002": "gemini",
	"6f18fce3-b9d2-4d34-9877-68f80b37e8a7": "groq",
	"c8b0b87a-a6d7-4f1d-84c5-6b3946c9fa20": "mistral",
	"deb879c3-0843-435c-aa06-38e8a4949be6": "ollama",
	"d2803e2c-5ae8-4496-a264-c979c5be5d30": "openai",
	"8980aa09-9a05-42a1-8ccc-f9fc02a4c4b0": "parasail",
	"de04d873-43dd-48c8-b2fb-e6b378f1ff10": "perplexity",
	"a861d23a-1b81-4c1a-bb7c-4b3ae53fc18f": "sgl",
	"63e75a9d-4319-4968-ae69-960589df3421": "vertex",
	"b9f74b0c-6a15-4998-b8ac-0c676a460058": "openrouter",
	"a904b921-42e1-43e7-9655-85f88451c054": "elevenlabs",
	"9734abc7-136a-4be2-8a5d-d47b98e153ba": "huggingface",
	"c4db6e60-8b03-4d38-8a90-6407ad339ee4": "nebius",
	"c93fa8f1-4275-41ea-90e7-b01a8c1c4b80": "xai",
	"887bc31a-fce6-4d3d-a86f-0f023eac471e": "replicate",
	"1c651d32-ac21-4a6f-ae58-9bbc55c96e9a": "vllm",
	"cf63883b-4df7-4492-bfc1-653dc47931fa": "runway",
	"9b63b21b-174c-4f75-b43a-ca70385397cd": "fireworks",
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
