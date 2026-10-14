package tables

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRoutingAssociation_ScopeOrgID(t *testing.T) {
	orgID := "org-123"
	rule := &TableRoutingRule{
		ScopeOrgID: &orgID,
	}
	require.NoError(t, rule.NormalizeRoutingAssociation())
	assert.Equal(t, "org:"+orgID, rule.RoutingRulesCacheKey())
	assert.Equal(t, orgID, rule.RoutingScopeOrgID())
	assert.Equal(t, "org", rule.RoutingScopeName())
}

func TestNormalizeRoutingAssociation_ScopeOrgUnitID(t *testing.T) {
	orgUnitID := "ou-123"
	rule := &TableRoutingRule{
		ScopeOrgUnitID: &orgUnitID,
	}
	require.NoError(t, rule.NormalizeRoutingAssociation())
	assert.Equal(t, "org_unit:"+orgUnitID, rule.RoutingRulesCacheKey())
	assert.Equal(t, "org_unit", rule.RoutingScopeName())
	assert.Equal(t, orgUnitID, rule.RoutingScopeID())
}

func TestNormalizeRoutingAssociation_RejectsScopeOrgAndOrgUnit(t *testing.T) {
	orgID := "org-123"
	orgUnitID := "ou-123"
	rule := &TableRoutingRule{
		ScopeOrgID:     &orgID,
		ScopeOrgUnitID: &orgUnitID,
	}
	err := rule.NormalizeRoutingAssociation()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope_org_id, scope_org_unit_id, and virtual_key_id")
}

func TestNormalizeRoutingAssociation_VirtualKeyID(t *testing.T) {
	vkID := "vk-456"
	rule := &TableRoutingRule{
		VirtualKeyID: &vkID,
	}
	require.NoError(t, rule.NormalizeRoutingAssociation())
	assert.Equal(t, "virtual_key:"+vkID, rule.RoutingRulesCacheKey())
	assert.Equal(t, "virtual_key", rule.RoutingScopeName())
}

func TestNormalizeRoutingAssociation_RejectsScopeOrgAndVirtualKey(t *testing.T) {
	orgID := "org-123"
	vkID := "vk-456"
	rule := &TableRoutingRule{
		ScopeOrgID:   &orgID,
		VirtualKeyID: &vkID,
	}
	err := rule.NormalizeRoutingAssociation()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope_org_id, scope_org_unit_id, and virtual_key_id")
}

func TestUnmarshalJSON_LegacyScopeMapsToScopeOrgID(t *testing.T) {
	raw := `{
		"id": "1",
		"name": "legacy-org",
		"cel_expression": "true",
		"scope": "org",
		"scope_id": "org-legacy"
	}`
	var rule TableRoutingRule
	require.NoError(t, json.Unmarshal([]byte(raw), &rule))
	require.NotNil(t, rule.ScopeOrgID)
	assert.Equal(t, "org-legacy", *rule.ScopeOrgID)
	assert.Nil(t, rule.VirtualKeyID)
}

func TestUnmarshalJSON_LegacyOrgUnitScope(t *testing.T) {
	raw := `{
		"id": "3",
		"name": "legacy-org-unit",
		"cel_expression": "true",
		"scope": "org_unit",
		"scope_id": "ou-legacy"
	}`
	var rule TableRoutingRule
	require.NoError(t, json.Unmarshal([]byte(raw), &rule))
	require.NotNil(t, rule.ScopeOrgUnitID)
	assert.Equal(t, "ou-legacy", *rule.ScopeOrgUnitID)
	assert.Nil(t, rule.ScopeOrgID)
	assert.Nil(t, rule.VirtualKeyID)
}

func TestUnmarshalJSON_LegacyVirtualKeyScope(t *testing.T) {
	raw := `{
		"id": "2",
		"name": "legacy-vk",
		"cel_expression": "true",
		"scope": "virtual_key",
		"scope_id": "vk-legacy"
	}`
	var rule TableRoutingRule
	require.NoError(t, json.Unmarshal([]byte(raw), &rule))
	require.NotNil(t, rule.VirtualKeyID)
	assert.Equal(t, "vk-legacy", *rule.VirtualKeyID)
	assert.Nil(t, rule.ScopeOrgID)
}

func TestSemanticRoutingCELExpression(t *testing.T) {
	assert.True(t, IsSemanticRoutingCELExpression("semantic_routing == true"))
	assert.True(t, IsSemanticRoutingCELExpression(" Semantic_Routing == TRUE "))
	assert.True(t, IsSemanticRoutingCELExpression("semantic_routing==true"))
	assert.False(t, IsSemanticRoutingCELExpression("semantic_routing"))
	assert.False(t, IsSemanticRoutingCELExpression("semantic_routing == false"))
	assert.False(t, IsSemanticRoutingCELExpression("true"))
	assert.False(t, IsSemanticRoutingCELExpression("budget_used >= 50"))
	assert.False(t, IsSemanticRoutingCELExpression("semantic_routing == true && model == \"gpt-4\""))
}

func TestValidateSemanticRoutingRejectsChainRule(t *testing.T) {
	assert.NoError(t, ValidateSemanticRouting("budget_used >= 50", true))
	assert.NoError(t, ValidateSemanticRouting(CelExpressionSemanticRouting, false))
	assert.Error(t, ValidateSemanticRouting(CelExpressionSemanticRouting, true))
}
