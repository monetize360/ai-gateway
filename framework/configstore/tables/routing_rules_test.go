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
	assert.Contains(t, err.Error(), "scope_org_id and virtual_key_id")
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

func TestNormalizeRoutingAction(t *testing.T) {
	pin, err := NormalizeRoutingAction("")
	require.NoError(t, err)
	assert.Equal(t, RoutingRuleActionPin, pin)

	pin, err = NormalizeRoutingAction("PIN")
	require.NoError(t, err)
	assert.Equal(t, RoutingRuleActionPin, pin)

	semantic, err := NormalizeRoutingAction("semantic")
	require.NoError(t, err)
	assert.Equal(t, RoutingRuleActionSemantic, semantic)

	_, err = NormalizeRoutingAction("weighted")
	require.Error(t, err)
}

func TestValidateSemanticActionRejectsChainRule(t *testing.T) {
	assert.NoError(t, ValidateSemanticAction(RoutingRuleActionPin, true))
	assert.NoError(t, ValidateSemanticAction(RoutingRuleActionSemantic, false))
	assert.Error(t, ValidateSemanticAction(RoutingRuleActionSemantic, true))
}

func TestActionValueDefaultsToPin(t *testing.T) {
	assert.Equal(t, RoutingRuleActionPin, (*TableRoutingRule)(nil).ActionValue())
	assert.Equal(t, RoutingRuleActionPin, (&TableRoutingRule{}).ActionValue())
	assert.True(t, (&TableRoutingRule{Action: RoutingRuleActionSemantic}).IsSemanticAction())
	assert.False(t, (&TableRoutingRule{}).IsSemanticAction())
}
