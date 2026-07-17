package tables

import "strings"

// Provider access type picklist category (MPilot picklists/finops/provider_access_type.json).
const ProviderAccessTypePicklistCategoryID = "c4e8f1a2-9b3d-4e7c-8a5f-1d2e3f4a5b6c"

// Picklist item IDs for provider access type. Keep in sync with
// picklists/finops/provider_access_type.json.
const (
	ProviderAccessAllowedPicklistItemID = "a1a1a1a1-b2b2-4c3c-8d4d-111111111111"
	ProviderAccessBlockedPicklistItemID = "b2b2b2b2-c3c3-4d4d-9e5e-222222222222"
)

// Logical access-type codes used by the HTTP API and UI.
const (
	ProviderAccessAllowed = "allowed"
	ProviderAccessBlocked = "blocked"
)

var accessTypePicklistIDToName = map[string]string{
	ProviderAccessAllowedPicklistItemID: ProviderAccessAllowed,
	ProviderAccessBlockedPicklistItemID: ProviderAccessBlocked,
}

var accessTypeNameToPicklistID = map[string]string{
	ProviderAccessAllowed: ProviderAccessAllowedPicklistItemID,
	ProviderAccessBlocked: ProviderAccessBlockedPicklistItemID,
}

// AccessTypeNameFromPicklistItem returns "allowed" or "blocked" for a picklist item ID.
func AccessTypeNameFromPicklistItem(picklistItemID string) (string, bool) {
	name, ok := accessTypePicklistIDToName[strings.TrimSpace(picklistItemID)]
	return name, ok
}

// PicklistItemIDForAccessTypeName returns the picklist item ID for "allowed" or "blocked".
func PicklistItemIDForAccessTypeName(name string) (string, bool) {
	id, ok := accessTypeNameToPicklistID[strings.TrimSpace(strings.ToLower(name))]
	return id, ok
}

// NormalizeAccessTypeToPicklistItem accepts either a picklist item UUID or a logical
// name ("allowed"/"blocked") and returns the canonical picklist item UUID.
func NormalizeAccessTypeToPicklistItem(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if _, ok := accessTypePicklistIDToName[value]; ok {
		return value, true
	}
	return PicklistItemIDForAccessTypeName(value)
}

// IsAllowedAccessType reports whether the stored access_type (UUID or name) means allowed.
func IsAllowedAccessType(value string) bool {
	id, ok := NormalizeAccessTypeToPicklistItem(value)
	return ok && id == ProviderAccessAllowedPicklistItemID
}

// IsBlockedAccessType reports whether the stored access_type (UUID or name) means blocked.
func IsBlockedAccessType(value string) bool {
	id, ok := NormalizeAccessTypeToPicklistItem(value)
	return ok && id == ProviderAccessBlockedPicklistItemID
}
