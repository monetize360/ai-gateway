package configstore

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

func TestGovernanceRefreshDeltaIsEmpty(t *testing.T) {
	require.True(t, (*GovernanceRefreshDelta)(nil).IsEmpty())
	require.True(t, (&GovernanceRefreshDelta{}).IsEmpty())
	require.False(t, (&GovernanceRefreshDelta{
		Organizations: []configstoreTables.TableOrganization{{ID: "org-1"}},
	}).IsEmpty())
}

func TestProviderConfigRefreshDeltaIsEmpty(t *testing.T) {
	require.True(t, (*ProviderConfigRefreshDelta)(nil).IsEmpty())
	require.True(t, (&ProviderConfigRefreshDelta{Changed: map[schemas.ModelProvider]ProviderConfig{}}).IsEmpty())
	require.False(t, (&ProviderConfigRefreshDelta{
		Changed: map[schemas.ModelProvider]ProviderConfig{schemas.OpenAI: {}},
	}).IsEmpty())
	require.False(t, (&ProviderConfigRefreshDelta{
		Removed: []schemas.ModelProvider{schemas.OpenAI},
	}).IsEmpty())
}

func TestRefreshOverlapIsPositive(t *testing.T) {
	require.Greater(t, RefreshOverlap, time.Duration(0))
}

func TestNormalizeRefreshSinceUsesUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	local := time.Date(2026, 6, 9, 10, 0, 0, 0, loc)
	normalized := NormalizeRefreshSince(local)
	require.Equal(t, time.UTC, normalized.Location())
	require.Equal(t, local.UTC(), normalized)
}
