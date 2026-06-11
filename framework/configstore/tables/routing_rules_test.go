package tables

import (
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
	assert.Equal(t, "org", rule.Scope)
	require.NotNil(t, rule.ScopeID)
	assert.Equal(t, orgID, *rule.ScopeID)
	assert.Equal(t, "org:"+orgID, rule.RoutingRulesCacheKey())
	assert.Equal(t, orgID, rule.RoutingScopeOrgID())
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

func TestHydrateAssociationFromLegacy_OrgScopeUsesScopeOrgID(t *testing.T) {
	scopeID := "org-legacy"
	rule := &TableRoutingRule{
		Scope:   "org",
		ScopeID: &scopeID,
	}
	rule.HydrateAssociationFromLegacy()
	require.NotNil(t, rule.ScopeOrgID)
	assert.Equal(t, scopeID, *rule.ScopeOrgID)
	assert.Nil(t, rule.OrgID)
}
