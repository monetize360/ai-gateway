package logging

import (
	"testing"

	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyVirtualKeyOrgToEntry_VKAndGovernanceOrgPresent(t *testing.T) {
	entry := &logstore.Log{}
	governanceOrgID := "org-usage-scope-123"
	vkID := "vk-456"

	applyVirtualKeyOrgToEntry(entry, vkID, governanceOrgID)

	require.NotNil(t, entry.ScopeOrgID)
	assert.Equal(t, governanceOrgID, *entry.ScopeOrgID)
}

func TestApplyVirtualKeyOrgToEntry_VKPresentNoGovernanceOrg(t *testing.T) {
	entry := &logstore.Log{VirtualKeyID: strPtr("vk-456")}

	applyVirtualKeyOrgToEntry(entry, "vk-456", "")

	assert.Nil(t, entry.ScopeOrgID)
}

func TestApplyVirtualKeyOrgToEntry_NoVKGovernanceOrgInContext(t *testing.T) {
	entry := &logstore.Log{}

	applyVirtualKeyOrgToEntry(entry, "", "jwt-org-only")

	assert.Nil(t, entry.ScopeOrgID)
}

func TestApplyOutputFieldsToEntry_SetsScopeOrgWhenVKUsed(t *testing.T) {
	entry := &logstore.Log{}
	governanceOrgID := "resolved-governance-org"
	vkID := "vk-789"

	applyOutputFieldsToEntry(entry, "key-id", "key-name", vkID, "vk-name", governanceOrgID, "", "", "", "", "", "", "", "", "", 0, 0, nil)

	require.NotNil(t, entry.VirtualKeyID)
	assert.Equal(t, vkID, *entry.VirtualKeyID)
	require.NotNil(t, entry.ScopeOrgID)
	assert.Equal(t, governanceOrgID, *entry.ScopeOrgID)
}

func TestApplyOutputFieldsToEntry_SetsRoutingQueryWhenPresent(t *testing.T) {
	entry := &logstore.Log{}
	paramsJSON := `{"region":"us-east-1","env":"prod"}`
	applyOutputFieldsToEntry(entry, "", "", "", "", "", "rule-1", "rule-name", paramsJSON, "", "", "", "", "", "", 0, 0, nil)
	require.NotNil(t, entry.RoutingQueryParams)
	assert.Equal(t, paramsJSON, *entry.RoutingQueryParams)
}

func TestApplyOutputFieldsToEntry_SetsRoutingSourceModelIDsWhenPresent(t *testing.T) {
	entry := &logstore.Log{}
	sourceProviderID := "11111111-1111-1111-1111-111111111101"
	sourceModelID := "22222222-2222-2222-2222-222222222201"
	applyOutputFieldsToEntry(entry, "", "", "", "", "", "rule-1", "rule-name", "", sourceProviderID, sourceModelID, "", "", "", "", 0, 0, nil)
	require.NotNil(t, entry.RoutingSourceProviderID)
	assert.Equal(t, sourceProviderID, *entry.RoutingSourceProviderID)
	require.NotNil(t, entry.RoutingSourceModelID)
	assert.Equal(t, sourceModelID, *entry.RoutingSourceModelID)
}

func TestApplyOutputFieldsToEntry_SetsGovernanceDecisionWhenPresent(t *testing.T) {
	entry := &logstore.Log{}
	applyOutputFieldsToEntry(entry, "", "", "", "", "", "", "", "", "", "", "budget_exceeded", "", "", "", 0, 0, nil)
	require.NotNil(t, entry.GovernanceDecision)
	assert.Equal(t, "budget_exceeded", *entry.GovernanceDecision)
}

func TestApplyOutputFieldsToEntry_NoGovernanceDecisionWhenEmpty(t *testing.T) {
	entry := &logstore.Log{}
	applyOutputFieldsToEntry(entry, "", "", "", "", "", "", "", "", "", "", "", "", "", "", 0, 0, nil)
	assert.Nil(t, entry.GovernanceDecision)
}

func strPtr(s string) *string {
	return &s
}
