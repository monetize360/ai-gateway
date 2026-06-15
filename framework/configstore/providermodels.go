package configstore

import (
	"context"
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// ModelNamesFromListResponse extracts unique model names from a provider ListModels response.
func ModelNamesFromListResponse(provider schemas.ModelProvider, listResp *schemas.BifrostListModelsResponse) []string {
	if listResp == nil || len(listResp.Data) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(listResp.Data))
	names := make([]string, 0, len(listResp.Data))
	for _, model := range listResp.Data {
		_, name := schemas.ParseModelString(model.ID, provider)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// ConfigModelTokenPricing carries per-token rates to persist on config_models rows.
type ConfigModelTokenPricing struct {
	InputCostPerToken  *float64
	OutputCostPerToken *float64
}

func configModelPricingUpdates(ctx context.Context, pricing *ConfigModelTokenPricing) map[string]any {
	updates := map[string]any{
		"updated_at": time.Now().UTC(),
	}
	if pricing != nil {
		updates["input_cost_per_token"] = pricing.InputCostPerToken
		updates["output_cost_per_token"] = pricing.OutputCostPerToken
	} else {
		updates["input_cost_per_token"] = nil
		updates["output_cost_per_token"] = nil
	}
	if userID := auditUserID(ctx); userID != "" {
		updates["updated_by"] = userID
	}
	return updates
}

// SyncProviderModels upserts ListModels results into config_models for the given provider.
// Models no longer present in modelNames are soft-deleted. Empty modelNames is a no-op
// so a failed or empty upstream response does not wipe existing rows.
// tokenPricing maps model name to catalog-derived per-token rates (nil values mean unknown pricing).
func (s *RDBConfigStore) SyncProviderModels(ctx context.Context, provider schemas.ModelProvider, modelNames []string, tokenPricing map[string]ConfigModelTokenPricing, tx ...*gorm.DB) error {
	if len(modelNames) == 0 {
		return nil
	}

	syncFn := func(txDB *gorm.DB) error {
		if !txDB.Migrator().HasTable(&tables.TableModel{}) {
			return nil
		}

		var dbProvider tables.TableProvider
		if err := scopeProviderByRuntimeKey(ActiveRows(txDB.WithContext(ctx)), provider).First(&dbProvider).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		desired := make(map[string]struct{}, len(modelNames))
		for _, name := range modelNames {
			desired[name] = struct{}{}
		}

		var existing []tables.TableModel
		if err := txDB.WithContext(ctx).Unscoped().Where("provider_id = ?", dbProvider.ID).Find(&existing).Error; err != nil {
			return err
		}

		existingByName := make(map[string]*tables.TableModel, len(existing))
		for i := range existing {
			existingByName[existing[i].Name] = &existing[i]
		}

		for name := range desired {
			pricing := tokenPricing[name]
			row, ok := existingByName[name]
			if ok {
				updates := configModelPricingUpdates(ctx, &pricing)
				if row.Deleted {
					updates["deleted"] = false
				}
				if err := txDB.WithContext(ctx).Model(&tables.TableModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
					return err
				}
				continue
			}

			model := &tables.TableModel{
				ProviderID:         dbProvider.ID,
				Name:               name,
				InputCostPerToken:  pricing.InputCostPerToken,
				OutputCostPerToken: pricing.OutputCostPerToken,
			}
			EnsureGovernanceRowID(&model.ID)
			ApplyAuditOnCreate(ctx, &model.SystemColumns)
			if err := txDB.WithContext(ctx).Create(model).Error; err != nil {
				return err
			}
		}

		for name, row := range existingByName {
			if row.Deleted {
				continue
			}
			if _, keep := desired[name]; keep {
				continue
			}
			if err := MarkDeleted(ctx, txDB, &tables.TableModel{}, "id = ?", row.ID); err != nil {
				return err
			}
		}

		return nil
	}

	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(syncFn)
	}
	return syncFn(tx[0])
}

// GetConfigModels returns all active config_models rows.
func (s *RDBConfigStore) GetConfigModels(ctx context.Context) ([]tables.TableModel, error) {
	if !s.DB().WithContext(ctx).Migrator().HasTable(&tables.TableModel{}) {
		return nil, nil
	}
	var models []tables.TableModel
	if err := ActiveRows(s.DB().WithContext(ctx)).Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}
