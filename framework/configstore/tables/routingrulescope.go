package tables

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// RoutingRuleAssociationQuery filters rows that share the same scope_org/vk/global bucket as rule.
func RoutingRuleAssociationQuery(db *gorm.DB, rule *TableRoutingRule) (*gorm.DB, error) {
	if rule == nil {
		return nil, fmt.Errorf("routing rule is nil")
	}
	if err := rule.NormalizeRoutingAssociation(); err != nil {
		return nil, err
	}
	if isNonEmptyString(rule.VirtualKeyID) {
		return db.Where("virtual_key_id = ?", strings.TrimSpace(*rule.VirtualKeyID)), nil
	}
	if isNonEmptyString(rule.ScopeOrgID) {
		return db.Where("scope_org_id = ?", strings.TrimSpace(*rule.ScopeOrgID)), nil
	}
	return db.Where("(scope_org_id IS NULL OR scope_org_id::text = '') AND (virtual_key_id IS NULL OR virtual_key_id::text = '')"), nil
}

// RoutingRulesByScopeQuery filters routing rules for a routing-engine scope level.
func RoutingRulesByScopeQuery(db *gorm.DB, scope, scopeID string) (*gorm.DB, error) {
	scope = strings.TrimSpace(scope)
	scopeID = strings.TrimSpace(scopeID)
	switch scope {
	case "", "global":
		return db.Where("(scope_org_id IS NULL OR scope_org_id::text = '') AND (virtual_key_id IS NULL OR virtual_key_id::text = '')"), nil
	case "virtual_key":
		if scopeID == "" {
			return nil, fmt.Errorf("virtual_key_id is required for virtual_key scope")
		}
		return db.Where("virtual_key_id = ?", scopeID), nil
	case "org", "team", "customer":
		if scopeID == "" {
			return nil, fmt.Errorf("scope_org_id is required for org scope")
		}
		return db.Where("scope_org_id = ?", scopeID), nil
	default:
		return nil, fmt.Errorf("unsupported routing scope %q", scope)
	}
}
