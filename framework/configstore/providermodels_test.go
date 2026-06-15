package configstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelNamesFromListResponse(t *testing.T) {
	t.Parallel()

	names := ModelNamesFromListResponse(schemas.OpenAI, &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "openai/gpt-4o"},
			{ID: "openai/gpt-4o-mini"},
			{ID: "gpt-4o"},
		},
	})
	assert.Equal(t, []string{"gpt-4o", "gpt-4o-mini"}, names)
}

func TestSyncProviderModels(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableModel{}))

	ctx := context.Background()
	providerID := uuid.NewString()
	require.NoError(t, store.DB().Create(&tables.TableProvider{
		ID:   providerID,
		Name: string(schemas.OpenAI),
	}).Error)

	err := store.SyncProviderModels(ctx, schemas.OpenAI, []string{"gpt-4o", "gpt-4o-mini"}, nil)
	require.NoError(t, err)

	var models []tables.TableModel
	require.NoError(t, ActiveRows(store.DB()).Where("provider_id = ?", providerID).Order("name ASC").Find(&models).Error)
	require.Len(t, models, 2)
	assert.Equal(t, "gpt-4o", models[0].Name)
	assert.Equal(t, "gpt-4o-mini", models[1].Name)

	err = store.SyncProviderModels(ctx, schemas.OpenAI, []string{"gpt-4o", "gpt-4.1"}, nil)
	require.NoError(t, err)

	require.NoError(t, ActiveRows(store.DB()).Where("provider_id = ?", providerID).Order("name ASC").Find(&models).Error)
	require.Len(t, models, 2)
	assert.Equal(t, "gpt-4.1", models[1].Name)

	var deleted tables.TableModel
	require.NoError(t, store.DB().Unscoped().Where("provider_id = ? AND name = ?", providerID, "gpt-4o-mini").First(&deleted).Error)
	assert.True(t, deleted.Deleted)

	err = store.SyncProviderModels(ctx, schemas.OpenAI, []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1"}, nil)
	require.NoError(t, err)

	require.NoError(t, ActiveRows(store.DB()).Where("provider_id = ?", providerID).Order("name ASC").Find(&models).Error)
	require.Len(t, models, 3)
}

func TestSyncProviderModels_PreservesExistingCostsWithoutCatalogPricing(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableModel{}))

	ctx := context.Background()
	providerID := uuid.NewString()
	require.NoError(t, store.DB().Create(&tables.TableProvider{
		ID:   providerID,
		Name: string(schemas.OpenAI),
	}).Error)

	inputCost := 0.00001
	outputCost := 0.00002
	require.NoError(t, store.DB().Create(&tables.TableModel{
		ID:                 uuid.NewString(),
		ProviderID:         providerID,
		Name:               "gpt-4o",
		InputCostPerToken:  &inputCost,
		OutputCostPerToken: &outputCost,
	}).Error)

	err := store.SyncProviderModels(ctx, schemas.OpenAI, []string{"gpt-4o"}, map[string]ConfigModelTokenPricing{
		"gpt-4o": {},
	})
	require.NoError(t, err)

	var model tables.TableModel
	require.NoError(t, ActiveRows(store.DB()).Where("provider_id = ? AND name = ?", providerID, "gpt-4o").First(&model).Error)
	require.NotNil(t, model.InputCostPerToken)
	require.NotNil(t, model.OutputCostPerToken)
	assert.Equal(t, inputCost, *model.InputCostPerToken)
	assert.Equal(t, outputCost, *model.OutputCostPerToken)
}

func TestSyncProviderModels_UpdatesCostsFromCatalogPricing(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableModel{}))

	ctx := context.Background()
	providerID := uuid.NewString()
	require.NoError(t, store.DB().Create(&tables.TableProvider{
		ID:   providerID,
		Name: string(schemas.OpenAI),
	}).Error)

	existingInput := 0.00001
	existingOutput := 0.00002
	require.NoError(t, store.DB().Create(&tables.TableModel{
		ID:                 uuid.NewString(),
		ProviderID:         providerID,
		Name:               "gpt-4o",
		InputCostPerToken:  &existingInput,
		OutputCostPerToken: &existingOutput,
	}).Error)

	catalogInput := 0.00003
	catalogOutput := 0.00004
	err := store.SyncProviderModels(ctx, schemas.OpenAI, []string{"gpt-4o"}, map[string]ConfigModelTokenPricing{
		"gpt-4o": {
			InputCostPerToken:  &catalogInput,
			OutputCostPerToken: &catalogOutput,
		},
	})
	require.NoError(t, err)

	var model tables.TableModel
	require.NoError(t, ActiveRows(store.DB()).Where("provider_id = ? AND name = ?", providerID, "gpt-4o").First(&model).Error)
	require.NotNil(t, model.InputCostPerToken)
	require.NotNil(t, model.OutputCostPerToken)
	assert.Equal(t, catalogInput, *model.InputCostPerToken)
	assert.Equal(t, catalogOutput, *model.OutputCostPerToken)
}

func TestSyncProviderModels_EmptyInputIsNoOp(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableModel{}))

	ctx := context.Background()
	providerID := uuid.NewString()
	require.NoError(t, store.DB().Create(&tables.TableProvider{
		ID:   providerID,
		Name: string(schemas.OpenAI),
	}).Error)
	require.NoError(t, store.DB().Create(&tables.TableModel{
		ID:         uuid.NewString(),
		ProviderID: providerID,
		Name:       "gpt-4o",
	}).Error)

	err := store.SyncProviderModels(ctx, schemas.OpenAI, nil, nil)
	require.NoError(t, err)

	var count int64
	require.NoError(t, ActiveRows(store.DB()).Model(&tables.TableModel{}).Where("provider_id = ?", providerID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
