package tables

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGovernanceScopeOrgID_PrefersScopeOrgID(t *testing.T) {
	scopeOrgID := "scope-org"
	orgID := "visibility-org"
	vk := &TableVirtualKey{
		ScopeOrgID: &scopeOrgID,
		OrgID:      &orgID,
	}

	require.NotNil(t, vk.GovernanceScopeOrgID())
	assert.Equal(t, scopeOrgID, *vk.GovernanceScopeOrgID())
	assert.Equal(t, scopeOrgID, vk.GovernanceScopeOrgIDString())
}

func TestGovernanceScopeOrgID_FallsBackToOrgID(t *testing.T) {
	orgID := "visibility-org"
	vk := &TableVirtualKey{OrgID: &orgID}

	require.NotNil(t, vk.GovernanceScopeOrgID())
	assert.Equal(t, orgID, *vk.GovernanceScopeOrgID())
	assert.Equal(t, orgID, vk.GovernanceScopeOrgIDString())
}

func TestGovernanceScopeOrgID_EmptyWhenUnset(t *testing.T) {
	vk := &TableVirtualKey{}
	assert.Nil(t, vk.GovernanceScopeOrgID())
	assert.Empty(t, vk.GovernanceScopeOrgIDString())
}
