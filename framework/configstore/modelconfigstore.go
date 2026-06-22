package configstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

func (s *RDBConfigStore) providerNameIndex(ctx context.Context) (map[string]string, error) {
	var providers []tables.TableProvider
	if err := ActiveRows(s.DB().WithContext(ctx)).Find(&providers).Error; err != nil {
		return nil, err
	}
	names := make(map[string]string, len(providers))
	for i := range providers {
		names[providers[i].ID] = providers[i].Name
	}
	return names, nil
}

func (s *RDBConfigStore) loadGovernanceConfigModels(ctx context.Context, query *gorm.DB) ([]tables.TableModel, error) {
	if query == nil {
		query = GovernanceActive(s.DB().WithContext(ctx))
	}
	var rows []tables.TableModel
	pre := governanceActivePreload()
	if err := query.Model(&tables.TableModel{}).
		Preload("Budgets", pre).
		Preload("RateLimits", pre).
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	filtered := make([]tables.TableModel, 0, len(rows))
	for i := range rows {
		if len(rows[i].Budgets) > 0 || len(rows[i].RateLimits) > 0 || strings.TrimSpace(rows[i].ConfigHash) != "" {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered, nil
}

func tableModelsToModelConfigs(rows []tables.TableModel, providerNames map[string]string) []tables.TableModelConfig {
	out := make([]tables.TableModelConfig, 0, len(rows))
	for i := range rows {
		var providerName *string
		if name, ok := providerNames[rows[i].ProviderID]; ok && name != "" {
			providerName = &name
		}
		if mc := tables.ModelConfigFromTableModel(&rows[i], providerName); mc != nil {
			out = append(out, *mc)
		}
	}
	return out
}

func (s *RDBConfigStore) tableModelToModelConfig(ctx context.Context, row *tables.TableModel) (*tables.TableModelConfig, error) {
	if row == nil {
		return nil, ErrNotFound
	}
	names, err := s.providerNameIndex(ctx)
	if err != nil {
		return nil, err
	}
	var providerName *string
	if name, ok := names[row.ProviderID]; ok && name != "" {
		providerName = &name
	}
	return tables.ModelConfigFromTableModel(row, providerName), nil
}

func (s *RDBConfigStore) resolveProviderID(ctx context.Context, txDB *gorm.DB, providerName string) (string, error) {
	if strings.TrimSpace(providerName) == "" {
		return "", fmt.Errorf("provider is required for model governance")
	}
	var provider tables.TableProvider
	if err := ActiveRows(txDB.WithContext(ctx)).First(&provider, "name = ?", providerName).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return provider.ID, nil
}

func (s *RDBConfigStore) findConfigModelRow(ctx context.Context, txDB *gorm.DB, providerID, modelName string) (*tables.TableModel, error) {
	var row tables.TableModel
	err := ActiveRows(txDB.WithContext(ctx)).
		Where("provider_id = ? AND name = ?", providerID, modelName).
		First(&row).Error
	if err == nil {
		return &row, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	row = tables.TableModel{
		ProviderID: providerID,
		Name:       modelName,
	}
	EnsureGovernanceRowID(&row.ID)
	ApplyAuditOnCreate(ctx, &row.SystemColumns)
	if err := txDB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, s.parseGormError(err)
	}
	return &row, nil
}

// GetModelConfigs retrieves config_models rows that carry governance limits.
func (s *RDBConfigStore) GetModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error) {
	rows, err := s.loadGovernanceConfigModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	names, err := s.providerNameIndex(ctx)
	if err != nil {
		return nil, err
	}
	return tableModelsToModelConfigs(rows, names), nil
}

// GetModelConfigsPaginated retrieves governed config_models with pagination and search.
func (s *RDBConfigStore) GetModelConfigsPaginated(ctx context.Context, params ModelConfigsQueryParams) ([]tables.TableModelConfig, int64, error) {
	baseQuery := GovernanceActive(s.DB().WithContext(ctx)).Model(&tables.TableModel{}).
		Where(`EXISTS (SELECT 1 FROM governance_budgets b WHERE b.model_config_id = config_models.id AND b.deleted = false)
			OR EXISTS (SELECT 1 FROM governance_rate_limits rl WHERE rl.model_config_id = config_models.id AND rl.deleted = false)
			OR COALESCE(config_models.config_hash, '') <> ''`)

	if params.Search != "" {
		search := "%" + strings.ToLower(params.Search) + "%"
		baseQuery = baseQuery.Where("LOWER(name) LIKE ?", search)
	}

	var totalCount int64
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, 0, err
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 25
	} else if limit > 100 {
		limit = 100
	}
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	var rows []tables.TableModel
	pre := governanceActivePreload()
	if err := baseQuery.
		Preload("Budgets", pre).
		Preload("RateLimits", pre).
		Order("created_at ASC, id ASC").
		Offset(offset).
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}

	names, err := s.providerNameIndex(ctx)
	if err != nil {
		return nil, 0, err
	}
	return tableModelsToModelConfigs(rows, names), totalCount, nil
}

// GetModelConfig retrieves governed config_models by model name and optional provider scope.
func (s *RDBConfigStore) GetModelConfig(ctx context.Context, modelName string, provider *string) (*tables.TableModelConfig, error) {
	names, err := s.providerNameIndex(ctx)
	if err != nil {
		return nil, err
	}
	providerIDByName := make(map[string]string, len(names))
	for id, name := range names {
		providerIDByName[name] = id
	}

	query := GovernanceActive(s.DB().WithContext(ctx)).Where("name = ?", modelName)
	if provider != nil && *provider != "" {
		providerID, ok := providerIDByName[*provider]
		if !ok {
			return nil, ErrNotFound
		}
		query = query.Where("provider_id = ?", providerID)
	}

	rows, err := s.loadGovernanceConfigModels(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return s.tableModelToModelConfig(ctx, &rows[0])
}

// GetModelConfigByID retrieves a governed config_models row by ID.
func (s *RDBConfigStore) GetModelConfigByID(ctx context.Context, id string) (*tables.TableModelConfig, error) {
	var row tables.TableModel
	pre := governanceActivePreload()
	if err := GovernanceActive(s.DB().WithContext(ctx)).
		Preload("Budgets", pre).
		Preload("RateLimits", pre).
		First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.tableModelToModelConfig(ctx, &row)
}

// CreateModelConfig ensures a config_models row exists and stores governance metadata on it.
func (s *RDBConfigStore) CreateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	if modelConfig == nil {
		return fmt.Errorf("model config cannot be nil")
	}
	if strings.TrimSpace(modelConfig.ModelName) == "" {
		return fmt.Errorf("model_name cannot be empty")
	}
	run := func(txDB *gorm.DB) error {
		if modelConfig.Provider == nil || strings.TrimSpace(*modelConfig.Provider) == "" {
			return fmt.Errorf("provider is required for model governance")
		}
		providerID, err := s.resolveProviderID(ctx, txDB, *modelConfig.Provider)
		if err != nil {
			return err
		}
		row, err := s.findConfigModelRow(ctx, txDB, providerID, modelConfig.ModelName)
		if err != nil {
			return err
		}
		if modelConfig.ID != "" && modelConfig.ID != row.ID {
			return fmt.Errorf("model config id does not match existing config_models row")
		}
		modelConfig.ID = row.ID
		tables.ApplyModelConfigToTableModel(modelConfig, row)
		ApplyAuditOnUpdate(ctx, &row.SystemColumns)
		return txDB.WithContext(ctx).Model(row).Updates(map[string]any{
			"config_hash": row.ConfigHash,
			"updated_at":  row.UpdatedAt,
		}).Error
	}
	if len(tx) > 0 {
		return run(tx[0])
	}
	return s.DB().WithContext(ctx).Transaction(run)
}

// UpdateModelConfig updates governance metadata on an existing config_models row.
func (s *RDBConfigStore) UpdateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	if modelConfig == nil {
		return fmt.Errorf("model config cannot be nil")
	}
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateModelConfig(ctx, modelConfig, transaction)
		})
	}

	txDB := tx[0]
	var existing tables.TableModel
	if err := dbForUpdate(GovernanceActive(txDB.WithContext(ctx))).First(&existing, "id = ?", modelConfig.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}

	if modelConfig.ModelName != "" && modelConfig.ModelName != existing.Name {
		existing.Name = modelConfig.ModelName
	}
	if modelConfig.Provider != nil && strings.TrimSpace(*modelConfig.Provider) != "" {
		providerID, err := s.resolveProviderID(ctx, txDB, *modelConfig.Provider)
		if err != nil {
			return err
		}
		existing.ProviderID = providerID
	}
	existing.ConfigHash = modelConfig.ConfigHash
	ApplyAuditOnUpdate(ctx, &existing.SystemColumns)
	if err := txDB.WithContext(ctx).Save(&existing).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateModelConfigs updates multiple governed config_models rows.
func (s *RDBConfigStore) UpdateModelConfigs(ctx context.Context, modelConfigs []*tables.TableModelConfig, tx ...*gorm.DB) error {
	if len(tx) == 0 {
		return s.DB().WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			return s.UpdateModelConfigs(ctx, modelConfigs, transaction)
		})
	}

	txDB := tx[0]
	sorted := append([]*tables.TableModelConfig(nil), modelConfigs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, mc := range sorted {
		if err := s.UpdateModelConfig(ctx, mc, txDB); err != nil {
			return err
		}
	}
	return nil
}

// TableModelsFromModelConfigs converts API model configs into config_models rows for in-memory hydration.
func TableModelsFromModelConfigs(modelConfigs []tables.TableModelConfig, providers []tables.TableProvider) []tables.TableModel {
	providerIDByName := make(map[string]string, len(providers))
	for i := range providers {
		providerIDByName[providers[i].Name] = providers[i].ID
	}
	out := make([]tables.TableModel, 0, len(modelConfigs))
	for i := range modelConfigs {
		mc := modelConfigs[i]
		row := tables.TableModel{
			ID:            mc.ID,
			Name:          mc.ModelName,
			Budgets:       mc.Budgets,
			RateLimits:    mc.RateLimits,
			ConfigHash:    mc.ConfigHash,
			SystemColumns: mc.SystemColumns,
		}
		if mc.Provider != nil {
			row.ProviderID = providerIDByName[*mc.Provider]
			row.ProviderName = *mc.Provider
		}
		out = append(out, row)
	}
	return out
}

// DeleteModelConfig removes governance limits from a config_models row without deleting the catalog row.
func (s *RDBConfigStore) DeleteModelConfig(ctx context.Context, id string) error {
	return s.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row tables.TableModel
		if err := dbForUpdate(GovernanceActive(tx)).First(&row, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := MarkGovernanceDeleted(ctx, tx, &tables.TableBudget{}, "model_config_id = ?", id); err != nil {
			return err
		}
		if err := MarkGovernanceDeleted(ctx, tx, &tables.TableRateLimit{}, "model_config_id = ?", id); err != nil {
			return err
		}
		return tx.Model(&row).Update("config_hash", "").Error
	})
}
