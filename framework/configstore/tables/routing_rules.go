package tables

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/maximhq/bifrost/core"
	"gorm.io/gorm"
)

// TableRoutingRule represents a routing rule in the database.
// Association is expressed with org_id and/or virtual_key_id (both nil = global).
// Legacy scope/scope_id columns are kept in sync for older rows and DB tooling.
type TableRoutingRule struct {
	ID            string `gorm:"primaryKey;type:varchar(255)" json:"id"`
	ConfigHash    string `gorm:"type:varchar(255)" json:"config_hash"` // Hash of config.json version, used for change detection
	Name          string `gorm:"type:varchar(255);not null;uniqueIndex:idx_routing_rule_name_association" json:"name"`
	Description   string `gorm:"type:text" json:"description"`
	Enabled       *bool  `gorm:"not null;default:true" json:"enabled,omitempty"` // nil = DB default (true); use EnabledValue() to read
	CelExpression string `gorm:"type:text;not null" json:"cel_expression"`

	// Routing Targets (output) — 1:many relationship; weights must sum to 1
	Targets []TableRoutingTarget `gorm:"foreignKey:RuleID;constraint:OnDelete:CASCADE" json:"targets"`

	Fallbacks       *string  `gorm:"type:text" json:"-"`           // JSON array of fallback chains
	ParsedFallbacks []string `gorm:"-" json:"fallbacks,omitempty"` // Parsed fallbacks from JSON

	Query       *string        `gorm:"type:text" json:"-"`
	ParsedQuery map[string]any `gorm:"-" json:"query,omitempty"`

	OrgID        *string `gorm:"type:uuid;uniqueIndex:idx_routing_rule_name_association" json:"org_id,omitempty"`
	VirtualKeyID *string `gorm:"type:uuid;uniqueIndex:idx_routing_rule_name_association" json:"virtual_key_id,omitempty"`

	// Legacy scope columns — synced from org_id/virtual_key_id on save; hydrated on load.
	Scope   string  `gorm:"type:varchar(50);not null" json:"-"`
	ScopeID *string `gorm:"type:varchar(255)" json:"-"`

	// Chaining
	ChainRule bool `gorm:"not null;default:false" json:"chain_rule"` // If true, re-evaluates routing chain after this rule matches

	// Execution
	Priority int `gorm:"type:int;not null;default:0;index" json:"priority"` // Lower = evaluated first within scope

	// Timestamps
	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`
}

// TableName for TableRoutingRule
func (TableRoutingRule) TableName() string { return "routing_rules" }

// EnabledValue returns the effective Enabled bool, treating nil as true (DB default).
func (r *TableRoutingRule) EnabledValue() bool {
	if r == nil {
		return false
	}
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

// HydrateAssociationFromLegacy populates org_id/virtual_key_id from legacy scope columns.
func (r *TableRoutingRule) HydrateAssociationFromLegacy() {
	if r == nil {
		return
	}
	if isNonEmptyString(r.OrgID) || isNonEmptyString(r.VirtualKeyID) {
		return
	}
	switch r.Scope {
	case "org", "team", "customer":
		if r.ScopeID != nil && *r.ScopeID != "" {
			r.OrgID = r.ScopeID
		}
	case "virtual_key":
		if r.ScopeID != nil && *r.ScopeID != "" {
			r.VirtualKeyID = r.ScopeID
		}
	}
}

// SyncLegacyScopeFields writes legacy scope/scope_id from org_id/virtual_key_id.
func (r *TableRoutingRule) SyncLegacyScopeFields() {
	if r == nil {
		return
	}
	if isNonEmptyString(r.VirtualKeyID) {
		r.Scope = "virtual_key"
		r.ScopeID = r.VirtualKeyID
		return
	}
	if isNonEmptyString(r.OrgID) {
		r.Scope = "org"
		id := strings.TrimSpace(*r.OrgID)
		r.ScopeID = &id
		return
	}
	r.Scope = "global"
	r.ScopeID = nil
}

// NormalizeRoutingAssociation validates org_id vs virtual_key_id and syncs legacy scope fields.
func (r *TableRoutingRule) NormalizeRoutingAssociation() error {
	if r == nil {
		return fmt.Errorf("routing rule is nil")
	}
	r.HydrateAssociationFromLegacy()
	if isNonEmptyString(r.OrgID) && isNonEmptyString(r.VirtualKeyID) {
		return fmt.Errorf("routing rule cannot specify both org_id and virtual_key_id")
	}
	r.SyncLegacyScopeFields()
	return nil
}

// RoutingRulesCacheKey returns the in-memory index key: global:, org:{id}, or virtual_key:{id}.
func (r *TableRoutingRule) RoutingRulesCacheKey() string {
	if r == nil {
		return "global:"
	}
	r.HydrateAssociationFromLegacy()
	if isNonEmptyString(r.VirtualKeyID) {
		return "virtual_key:" + strings.TrimSpace(*r.VirtualKeyID)
	}
	if isNonEmptyString(r.OrgID) {
		return "org:" + strings.TrimSpace(*r.OrgID)
	}
	return "global:"
}

// RoutingScopeName returns the scope level used by the routing engine.
func (r *TableRoutingRule) RoutingScopeName() string {
	switch r.RoutingRulesCacheKey() {
	case "global:":
		return "global"
	default:
		if strings.HasPrefix(r.RoutingRulesCacheKey(), "virtual_key:") {
			return "virtual_key"
		}
		return "org"
	}
}

func isNonEmptyString(s *string) bool {
	return s != nil && strings.TrimSpace(*s) != ""
}

// UnmarshalJSON accepts org_id/virtual_key_id or legacy scope/scope_id from config.json.
func (r *TableRoutingRule) UnmarshalJSON(data []byte) error {
	type Alias TableRoutingRule
	type legacyRoutingRule struct {
		Alias
		Scope   string  `json:"scope"`
		ScopeID *string `json:"scope_id"`
	}
	var temp legacyRoutingRule
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	*r = TableRoutingRule(temp.Alias)
	if temp.Scope != "" {
		r.Scope = temp.Scope
	}
	if temp.ScopeID != nil {
		r.ScopeID = temp.ScopeID
	}
	r.HydrateAssociationFromLegacy()
	r.SyncLegacyScopeFields()
	return nil
}

// BeforeSave hook for TableRoutingRule to serialize JSON fields
func (r *TableRoutingRule) BeforeSave(tx *gorm.DB) error {
	if err := r.NormalizeRoutingAssociation(); err != nil {
		return err
	}
	if len(r.ParsedFallbacks) > 0 {
		data, err := sonic.Marshal(r.ParsedFallbacks)
		if err != nil {
			return err
		}
		r.Fallbacks = bifrost.Ptr(string(data))
	} else {
		r.Fallbacks = nil
	}
	if r.ParsedQuery != nil {
		data, err := sonic.Marshal(r.ParsedQuery)
		if err != nil {
			return err
		}
		r.Query = bifrost.Ptr(string(data))
	} else {
		r.Query = nil
	}
	return nil
}

// AfterFind hook for TableRoutingRule to deserialize JSON fields
func (r *TableRoutingRule) AfterFind(tx *gorm.DB) error {
	if r.Fallbacks != nil && strings.TrimSpace(*r.Fallbacks) != "" {
		if err := sonic.Unmarshal([]byte(*r.Fallbacks), &r.ParsedFallbacks); err != nil {
			return err
		}
	}
	if r.Query != nil && strings.TrimSpace(*r.Query) != "" {
		if err := sonic.Unmarshal([]byte(*r.Query), &r.ParsedQuery); err != nil {
			return err
		}
	}
	r.HydrateAssociationFromLegacy()
	return nil
}

// TableRoutingTarget represents a weighted routing target for probabilistic routing.
// Multiple targets can be associated with a single routing rule; weights determine
// the probability of each target being selected and must sum to 1 across all targets in a rule.
// The composite (RuleID, Provider, Model, KeyID) is unique to prevent duplicate target configs.
type TableRoutingTarget struct {
	RuleID          string  `gorm:"type:varchar(255);not null;index;uniqueIndex:idx_routing_target_config" json:"-"`
	Provider        *string `gorm:"type:varchar(255);uniqueIndex:idx_routing_target_config" json:"provider,omitempty"` // nil = use incoming provider
	Model           *string `gorm:"type:varchar(255);uniqueIndex:idx_routing_target_config" json:"model,omitempty"`    // nil = use incoming model
	KeyID           *string `gorm:"type:varchar(255);uniqueIndex:idx_routing_target_config" json:"key_id,omitempty"`   // persisted key pin
	ProviderKeyName *string `gorm:"-" json:"provider_key_name,omitempty"`                                              // config-only alias; resolved to key_id during load
	Weight          float64 `gorm:"not null;default:1" json:"weight"`                                                  // must sum to 1 across all targets in a rule
}

// TableName for TableRoutingTarget
func (TableRoutingTarget) TableName() string { return "routing_targets" }
