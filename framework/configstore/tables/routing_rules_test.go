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
