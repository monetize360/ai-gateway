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
// Routing scope is expressed with scope_org_id and/or virtual_key_id (both nil = global).
// org_id is reserved for MPilot tenant visibility and is not used by the routing engine.
// Legacy scope/scope_id columns are kept in sync for older rows and DB tooling.
type TableRoutingRule struct {
	ID            string `gorm:"primaryKey;type:varchar(255)" json:"id"`
	ConfigHash    string `gorm:"type:varchar(255)" json:"config_hash"` // Hash of config.json version, used for change detection
	Name          string `gorm:"type:varchar(255);not null;uniqueIndex:idx_routing_rule_name_association" json:"name"`
	Description   string `gorm:"type:text" json:"description"`
	Enabled       *bool  `gorm:"not null;default:true" json:"enabled,omitempty"` // nil = DB default (true); use EnabledValue() to read
	CelExpression string `gorm:"type:text;not null" json:"cel_expression"`

	// Routing output — nil provider/model means use the incoming request value.
	Provider        *string `gorm:"type:varchar(255)" json:"provider,omitempty"`
	Model           *string `gorm:"type:varchar(255)" json:"model,omitempty"`
	KeyID           *string `gorm:"type:varchar(255)" json:"key_id,omitempty"`
	ProviderKeyName *string `gorm:"-" json:"provider_key_name,omitempty"` // config-only alias; resolved to key_id during load

	Fallbacks       *string  `gorm:"type:text" json:"-"`           // JSON array of fallback chains
	ParsedFallbacks []string `gorm:"-" json:"fallbacks,omitempty"` // Parsed fallbacks from JSON

	Query       *string        `gorm:"type:text" json:"-"`
	ParsedQuery map[string]any `gorm:"-" json:"query,omitempty"`

	// OrgID is MPilot tenant visibility only; not evaluated by the routing engine.
	OrgID *string `gorm:"type:uuid;index" json:"org_id,omitempty"`

	// ScopeOrgID is the org this rule applies to at routing runtime (mutually exclusive with virtual_key_id).
	ScopeOrgID   *string `gorm:"type:uuid;uniqueIndex:idx_routing_rule_name_association" json:"scope_org_id,omitempty"`
	VirtualKeyID *string `gorm:"type:uuid;uniqueIndex:idx_routing_rule_name_association" json:"virtual_key_id,omitempty"`

	// Legacy scope columns — synced from scope_org_id/virtual_key_id on save; hydrated on load.
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

// HydrateAssociationFromLegacy populates scope_org_id/virtual_key_id from legacy scope columns.
func (r *TableRoutingRule) HydrateAssociationFromLegacy() {
	if r == nil {
		return
	}
	if isNonEmptyString(r.ScopeOrgID) || isNonEmptyString(r.VirtualKeyID) {
		return
	}
	switch r.Scope {
	case "org", "team", "customer":
		if r.ScopeID != nil && *r.ScopeID != "" {
			id := strings.TrimSpace(*r.ScopeID)
			r.ScopeOrgID = &id
		}
	case "virtual_key":
		if r.ScopeID != nil && *r.ScopeID != "" {
			id := strings.TrimSpace(*r.ScopeID)
			r.VirtualKeyID = &id
		}
	}
}

// SyncLegacyScopeFields writes legacy scope/scope_id from scope_org_id/virtual_key_id.
func (r *TableRoutingRule) SyncLegacyScopeFields() {
	if r == nil {
		return
	}
	if isNonEmptyString(r.VirtualKeyID) {
		r.Scope = "virtual_key"
		r.ScopeID = r.VirtualKeyID
		return
	}
	if isNonEmptyString(r.ScopeOrgID) {
		r.Scope = "org"
		id := strings.TrimSpace(*r.ScopeOrgID)
		r.ScopeID = &id
		return
	}
	r.Scope = "global"
	r.ScopeID = nil
}

// NormalizeRoutingAssociation validates scope_org_id vs virtual_key_id and syncs legacy scope fields.
func (r *TableRoutingRule) NormalizeRoutingAssociation() error {
	if r == nil {
		return fmt.Errorf("routing rule is nil")
	}
	r.HydrateAssociationFromLegacy()
	if isNonEmptyString(r.ScopeOrgID) && isNonEmptyString(r.VirtualKeyID) {
		return fmt.Errorf("routing rule cannot specify both scope_org_id and virtual_key_id")
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
	if isNonEmptyString(r.ScopeOrgID) {
		return "org:" + strings.TrimSpace(*r.ScopeOrgID)
	}
	return "global:"
}

// RoutingScopeOrgID returns the org ID used for routing scope, if any.
func (r *TableRoutingRule) RoutingScopeOrgID() string {
	if r == nil {
		return ""
	}
	r.HydrateAssociationFromLegacy()
	if !isNonEmptyString(r.ScopeOrgID) {
		return ""
	}
	return strings.TrimSpace(*r.ScopeOrgID)
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

type legacyRoutingTarget struct {
	Provider        *string `json:"provider"`
	Model           *string `json:"model"`
	KeyID           *string `json:"key_id"`
	ProviderKeyName *string `json:"provider_key_name"`
}

func applyLegacyRoutingTarget(rule *TableRoutingRule, target legacyRoutingTarget) {
	if rule == nil {
		return
	}
	if rule.Provider == nil && target.Provider != nil {
		rule.Provider = target.Provider
	}
	if rule.Model == nil && target.Model != nil {
		rule.Model = target.Model
	}
	if rule.KeyID == nil && target.KeyID != nil {
		rule.KeyID = target.KeyID
	}
	if rule.ProviderKeyName == nil && target.ProviderKeyName != nil {
		rule.ProviderKeyName = target.ProviderKeyName
	}
}

// UnmarshalJSON accepts inline provider/model/key_id, legacy targets[0], or scope/scope_id from config.json.
func (r *TableRoutingRule) UnmarshalJSON(data []byte) error {
	type Alias TableRoutingRule
	type legacyRoutingRule struct {
		Alias
		Scope   string                `json:"scope"`
		ScopeID *string               `json:"scope_id"`
		Targets []legacyRoutingTarget `json:"targets"`
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
	if len(temp.Targets) > 0 {
		applyLegacyRoutingTarget(r, temp.Targets[0])
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
