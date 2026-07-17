package tables

import (
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/gorm"
)

// ProviderAccessPolicyRT holds the aggregated allow/block provider lists
// for runtime use on TableVirtualKey. Kept in the tables package to avoid
// import cycles; populated by configstore.AggregateProviderAccess.
type ProviderAccessPolicyRT struct {
	AllowedProviders     schemas.WhiteList `json:"allowed_providers"`
	BlacklistedProviders schemas.BlackList `json:"blacklisted_providers"`
}

// IsEmpty returns true when the policy imposes no restrictions.
func (p *ProviderAccessPolicyRT) IsEmpty() bool {
	return p == nil || (len(p.AllowedProviders) == 0 && len(p.BlacklistedProviders) == 0)
}

// TableProviderAccess is a scope-level provider allow/block rule.
// Each row targets one provider (via provider_id FK) or all providers
// when is_wildcard is true (provider_id must be NULL).
//
// Rows are scoped to exactly one of:
//   - VirtualKeyID (VK-level policy)
//   - ScopeOrgID   (org-level policy)
//
// Multiple rows per scope are aggregated by AggregateProviderAccess
// into ProviderAccessPolicy (AllowedProviders / BlacklistedProviders).
//
// AccessType stores the MPilot picklist item UUID (see provider_access_type.json).
// AccessTypeCode is the logical name ("allowed"|"blocked") for API/UI JSON.
type TableProviderAccess struct {
	ID           string  `gorm:"primaryKey;type:uuid" json:"id"`
	VirtualKeyID *string `gorm:"type:uuid;index" json:"virtual_key_id,omitempty"`
	ScopeOrgID   *string `gorm:"type:uuid;index" json:"scope_org_id,omitempty"`

	ProviderID     *string        `gorm:"type:uuid" json:"provider_id,omitempty"`
	ConfigProvider *TableProvider `gorm:"foreignKey:ProviderID;references:ID" json:"-"`

	AccessType     string `gorm:"column:access_type;type:uuid;not null" json:"access_type_id,omitempty"`
	AccessTypeCode string `gorm:"-" json:"access_type"` // "allowed" | "blocked"
	IsWildcard     bool   `gorm:"not null;default:false" json:"is_wildcard"`

	OrgID *string `gorm:"type:uuid;index" json:"org_id,omitempty"`

	SystemColumns
}

func (TableProviderAccess) TableName() string {
	return "governance_provider_access"
}

func (pa *TableProviderAccess) BeforeSave(tx *gorm.DB) error {
	vkSet := isNonEmptyString(pa.VirtualKeyID)
	orgSet := isNonEmptyString(pa.ScopeOrgID)
	if vkSet && orgSet {
		return fmt.Errorf("virtual_key_id and scope_org_id are mutually exclusive")
	}
	if !vkSet && !orgSet {
		return fmt.Errorf("either virtual_key_id or scope_org_id must be set")
	}
	if pa.IsWildcard && pa.ProviderID != nil && *pa.ProviderID != "" {
		return fmt.Errorf("provider_id must be NULL when is_wildcard is true")
	}
	if !pa.IsWildcard && (pa.ProviderID == nil || *pa.ProviderID == "") {
		return fmt.Errorf("provider_id is required when is_wildcard is false")
	}

	// Prefer explicit AccessTypeCode from API; fall back to AccessType (name or UUID).
	raw := pa.AccessTypeCode
	if raw == "" {
		raw = pa.AccessType
	}
	id, ok := NormalizeAccessTypeToPicklistItem(raw)
	if !ok {
		return fmt.Errorf("access_type must be 'allowed' or 'blocked' (or the corresponding picklist item id)")
	}
	pa.AccessType = id
	if name, ok := AccessTypeNameFromPicklistItem(id); ok {
		pa.AccessTypeCode = name
	}
	return nil
}

// AfterFind resolves the picklist item UUID into AccessTypeCode for API/UI.
func (pa *TableProviderAccess) AfterFind(tx *gorm.DB) error {
	if name, ok := AccessTypeNameFromPicklistItem(pa.AccessType); ok {
		pa.AccessTypeCode = name
	}
	return nil
}

// ProviderName returns the resolved provider name from the preloaded ConfigProvider,
// or empty string if not preloaded or wildcard.
func (pa *TableProviderAccess) ProviderName() string {
	if pa.ConfigProvider != nil {
		return pa.ConfigProvider.Name
	}
	return ""
}
