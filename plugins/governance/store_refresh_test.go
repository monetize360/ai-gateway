package governance

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type budgetRateLimitLoaderStub struct {
	budgets     []configstoreTables.TableBudget
	rateLimits  []configstoreTables.TableRateLimit
}

func (s *budgetRateLimitLoaderStub) GetBudgets(context.Context) ([]configstoreTables.TableBudget, error) {
	return s.budgets, nil
}

func (s *budgetRateLimitLoaderStub) GetRateLimits(context.Context) ([]configstoreTables.TableRateLimit, error) {
	return s.rateLimits, nil
}

func TestReloadBudgetRateLimitConfigs_RefreshesSoftLimit(t *testing.T) {
	ctx := context.Background()
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget-1", 10.0, 15.0, "1d")
	budget.SoftLimit = true

	store, err := NewLocalGovernanceStore(ctx, logger, nil, &configstore.GovernanceConfig{
		Budgets: []configstoreTables.TableBudget{*budget},
	}, nil)
	require.NoError(t, err)

	loader := &budgetRateLimitLoaderStub{
		budgets: []configstoreTables.TableBudget{
			func() configstoreTables.TableBudget {
				updated := *budget
				updated.SoftLimit = false
				return updated
			}(),
		},
	}

	require.NoError(t, store.reloadBudgetRateLimitConfigs(ctx, loader))

	loaded := store.LoadBudget(ctx, budget.ID)
	require.NotNil(t, loaded)
	assert.False(t, loaded.SoftLimit)

	decision, checkErr := store.CheckBudget(ctx, EntityWiseBudgets{
		"VK": {loaded},
	}, nil)
	require.Error(t, checkErr)
	assert.Equal(t, DecisionBudgetExceeded, decision)
}

func TestReloadBudgetRateLimitConfigs_PreservesInMemoryUsage(t *testing.T) {
	ctx := context.Background()
	logger := NewMockLogger()

	budget := buildBudgetWithUsage("budget-1", 100.0, 5.0, "1d")
	budget.SoftLimit = false

	store, err := NewLocalGovernanceStore(ctx, logger, nil, &configstore.GovernanceConfig{
		Budgets: []configstoreTables.TableBudget{*budget},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, store.BumpBudgetUsage(ctx, budget.ID, 7))

	loader := &budgetRateLimitLoaderStub{
		budgets: []configstoreTables.TableBudget{
			func() configstoreTables.TableBudget {
				updated := *budget
				updated.SoftLimit = true
				updated.CurrentUsage = 1 // DB value must not clobber in-memory usage
				return updated
			}(),
		},
	}

	require.NoError(t, store.reloadBudgetRateLimitConfigs(ctx, loader))

	loaded := store.LoadBudget(ctx, budget.ID)
	require.NotNil(t, loaded)
	assert.True(t, loaded.SoftLimit)
	assert.InDelta(t, 12.0, loaded.CurrentUsage, 0.0001)
}
