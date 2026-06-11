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

// SyncProviderModels upserts ListModels results into config_models for the given provider.
// Models no longer present in modelNames are soft-deleted. Empty modelNames is a no-op
// so a failed or empty upstream response does not wipe existing rows.
func (s *RDBConfigStore) SyncProviderModels(ctx context.Context, provider schemas.ModelProvider, modelNames []string, tx ...*gorm.DB) error {
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
			row, ok := existingByName[name]
			if ok {
				if row.Deleted {
					updates := map[string]any{
						"deleted":    false,
						"updated_at": time.Now().UTC(),
					}
					if userID := auditUserID(ctx); userID != "" {
						updates["updated_by"] = userID
					}
					if err := txDB.WithContext(ctx).Model(&tables.TableModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
						return err
					}
				}
				continue
			}

			model := &tables.TableModel{
				ProviderID: dbProvider.ID,
				Name:       name,
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
