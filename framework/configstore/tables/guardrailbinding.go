package tables

import "strings"

// TableGuardrailBinding attaches a policy to one caller scope and an optional provider/model.
// At most one of VirtualKeyID, ScopeOrgUnitID, ScopeOrgID is set. None means global.
type TableGuardrailBinding struct {
	ID                string  `gorm:"primaryKey;type:uuid" json:"id"`
	GuardrailPolicyID string  `gorm:"column:guardrail_policy_id;type:uuid;index;not null" json:"guardrail_policy_id"`
	VirtualKeyID      *string `gorm:"column:virtual_key_id;type:uuid;index" json:"virtual_key_id,omitempty"`
	ScopeOrgUnitID    *string `gorm:"column:scope_org_unit_id;type:uuid;index" json:"scope_org_unit_id,omitempty"`
	ScopeOrgID        *string `gorm:"column:scope_org_id;type:uuid;index" json:"scope_org_id,omitempty"`
	ProviderID        *string `gorm:"column:provider_id;type:uuid;index" json:"provider_id,omitempty"`
	ModelID           *string `gorm:"column:model_id;type:uuid;index" json:"model_id,omitempty"`
	Priority          *int    `gorm:"column:priority" json:"priority,omitempty"`
	SamplingRate      *int    `gorm:"column:sampling_rate" json:"sampling_rate,omitempty"`
	CelExpression     string  `gorm:"column:cel_expression;type:text" json:"cel_expression,omitempty"`
	Enabled           *bool   `gorm:"default:true" json:"enabled,omitempty"`
	OrgID             *string `gorm:"column:org_id;type:uuid" json:"org_id,omitempty"`

	SystemColumns
}

func (TableGuardrailBinding) TableName() string { return "guardrail_bindings" }

func (b *TableGuardrailBinding) EnabledValue() bool {
	if b == nil {
		return false
	}
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

func (b *TableGuardrailBinding) PriorityValue() int {
	if b == nil || b.Priority == nil {
		return 0
	}
	return *b.Priority
}

func (b *TableGuardrailBinding) SamplingRateValue() int {
	if b == nil || b.SamplingRate == nil {
		return 100
	}
	rate := *b.SamplingRate
	if rate < 0 {
		return 0
	}
	if rate > 100 {
		return 100
	}
	return rate
}

func (b *TableGuardrailBinding) trimmed(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func (b *TableGuardrailBinding) VirtualKey() string {
	if b == nil {
		return ""
	}
	return b.trimmed(b.VirtualKeyID)
}

func (b *TableGuardrailBinding) OrgUnit() string {
	if b == nil {
		return ""
	}
	return b.trimmed(b.ScopeOrgUnitID)
}

func (b *TableGuardrailBinding) Org() string {
	if b == nil {
		return ""
	}
	return b.trimmed(b.ScopeOrgID)
}

func (b *TableGuardrailBinding) IsGlobal() bool {
	return b != nil && b.VirtualKey() == "" && b.OrgUnit() == "" && b.Org() == ""
}
