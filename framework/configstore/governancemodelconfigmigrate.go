package configstore

import (
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// migrateGovernanceModelConfigsToConfigModels repoints model-level budget/rate-limit
// ownership from governance_model_configs to config_models, then drops the legacy table.
func migrateGovernanceModelConfigsToConfigModels(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable("governance_model_configs") {
		return nil
	}
	if !db.Migrator().HasTable("config_models") {
		return nil
	}

	dialect := db.Dialector.Name()

	// Provider-scoped rows: map governance_model_configs → config_models via provider name + model name.
	if sql := repointModelGovernanceSQL(dialect, false); sql != "" {
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("repoint provider-scoped model governance: %w", err)
		}
	}

	// Global rows (provider IS NULL): duplicate child budgets/rate limits onto every matching config_models row.
	if err := duplicateGlobalModelGovernance(db, dialect); err != nil {
		return err
	}

	if err := db.Exec("DROP TABLE IF EXISTS governance_model_configs").Error; err != nil {
		return fmt.Errorf("drop governance_model_configs: %w", err)
	}
	return nil
}

func repointModelGovernanceSQL(dialect string, global bool) string {
	providerClause := "gmc.provider IS NOT NULL AND gmc.provider = cp.name"
	if global {
		providerClause = "gmc.provider IS NULL"
	}
	switch dialect {
	case "postgres":
		return fmt.Sprintf(`
UPDATE governance_budgets AS b
SET model_config_id = cm.id
FROM governance_model_configs AS gmc
JOIN config_providers AS cp ON %s
JOIN config_models AS cm ON cm.provider_id = cp.id AND cm.name = gmc.model_name AND cm.deleted = false
WHERE b.model_config_id = gmc.id AND gmc.deleted = false`, providerClause) + ";" + fmt.Sprintf(`
UPDATE governance_rate_limits AS rl
SET model_config_id = cm.id
FROM governance_model_configs AS gmc
JOIN config_providers AS cp ON %s
JOIN config_models AS cm ON cm.provider_id = cp.id AND cm.name = gmc.model_name AND cm.deleted = false
WHERE rl.model_config_id = gmc.id AND gmc.deleted = false`, providerClause)
	case "sqlite":
		if global {
			return ""
		}
		return `
UPDATE governance_budgets
SET model_config_id = (
  SELECT cm.id
  FROM governance_model_configs gmc
  JOIN config_providers cp ON gmc.provider IS NOT NULL AND gmc.provider = cp.name
  JOIN config_models cm ON cm.provider_id = cp.id AND cm.name = gmc.model_name AND cm.deleted = 0
  WHERE gmc.id = governance_budgets.model_config_id AND gmc.deleted = 0
  LIMIT 1
)
WHERE model_config_id IN (SELECT id FROM governance_model_configs WHERE provider IS NOT NULL AND deleted = 0);
UPDATE governance_rate_limits
SET model_config_id = (
  SELECT cm.id
  FROM governance_model_configs gmc
  JOIN config_providers cp ON gmc.provider IS NOT NULL AND gmc.provider = cp.name
  JOIN config_models cm ON cm.provider_id = cp.id AND cm.name = gmc.model_name AND cm.deleted = 0
  WHERE gmc.id = governance_rate_limits.model_config_id AND gmc.deleted = 0
  LIMIT 1
)
WHERE model_config_id IN (SELECT id FROM governance_model_configs WHERE provider IS NOT NULL AND deleted = 0);`
	default:
		return ""
	}
}

func duplicateGlobalModelGovernance(db *gorm.DB, dialect string) error {
	type globalRow struct {
		ID        string
		ModelName string
	}
	var globals []globalRow
	q := `SELECT id, model_name FROM governance_model_configs WHERE provider IS NULL AND deleted = ` + deletedLiteral(dialect, false)
	if err := db.Raw(q).Scan(&globals).Error; err != nil {
		return fmt.Errorf("list global governance model configs: %w", err)
	}
	for _, gmc := range globals {
		type modelRow struct {
			ID string
		}
		var models []modelRow
		modelQ := `SELECT id FROM config_models WHERE name = ? AND deleted = ` + deletedLiteral(dialect, false)
		if err := db.Raw(modelQ, gmc.ModelName).Scan(&models).Error; err != nil {
			return fmt.Errorf("list config_models for global gmc %s: %w", gmc.ID, err)
		}
		for _, cm := range models {
			if cm.ID == "" {
				continue
			}
			if err := cloneChildGovernanceForModel(db, dialect, gmc.ID, cm.ID); err != nil {
				return err
			}
		}
		// Remove orphaned child rows still pointing at the global governance_model_configs row.
		if err := db.Exec(`DELETE FROM governance_budgets WHERE model_config_id = ?`, gmc.ID).Error; err != nil {
			return err
		}
		if err := db.Exec(`DELETE FROM governance_rate_limits WHERE model_config_id = ?`, gmc.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

func cloneChildGovernanceForModel(db *gorm.DB, dialect, fromModelConfigID, toModelConfigID string) error {
	if fromModelConfigID == toModelConfigID {
		return repointSingleModelConfigID(db, dialect, fromModelConfigID, toModelConfigID)
	}

	type budgetRow struct {
		MaxLimit      float64
		ResetDuration string
		LastReset     string
		CurrentUsage  float64
		SoftLimit     bool
		ConfigHash    *string
	}
	var budgets []budgetRow
	if err := db.Raw(`SELECT max_limit, reset_duration, last_reset, current_usage, soft_limit, config_hash
		FROM governance_budgets WHERE model_config_id = ? AND deleted = `+deletedLiteral(dialect, false), fromModelConfigID).Scan(&budgets).Error; err != nil {
		return err
	}
	for _, b := range budgets {
		newID := uuid.NewString()
		if dialect == "postgres" {
			if err := db.Exec(`INSERT INTO governance_budgets (id, max_limit, reset_duration, last_reset, current_usage, model_config_id, soft_limit, config_hash, created_at, updated_at, deleted)
				SELECT ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW(), false
				WHERE NOT EXISTS (
					SELECT 1 FROM governance_budgets
					WHERE model_config_id = ? AND reset_duration = ? AND deleted = false
				)`, newID, b.MaxLimit, b.ResetDuration, b.LastReset, b.CurrentUsage, toModelConfigID, b.SoftLimit, b.ConfigHash, toModelConfigID, b.ResetDuration).Error; err != nil {
				return err
			}
		} else {
			if err := db.Exec(`INSERT INTO governance_budgets (id, max_limit, reset_duration, last_reset, current_usage, model_config_id, soft_limit, config_hash, created_at, updated_at, deleted)
				SELECT ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'), 0
				WHERE NOT EXISTS (
					SELECT 1 FROM governance_budgets
					WHERE model_config_id = ? AND reset_duration = ? AND deleted = 0
				)`, newID, b.MaxLimit, b.ResetDuration, b.LastReset, b.CurrentUsage, toModelConfigID, b.SoftLimit, b.ConfigHash, toModelConfigID, b.ResetDuration).Error; err != nil {
				return err
			}
		}
	}

	type rlRow struct {
		TokenMaxLimit           *int64
		TokenResetDuration      *string
		TokenCurrentUsage       *int64
		TokenLastReset          *string
		RequestMaxLimit         *int64
		RequestResetDuration    *string
		RequestCurrentUsage     *int64
		RequestLastReset        *string
		SoftLimit               bool
		ConfigHash              *string
	}
	var rateLimits []rlRow
	if err := db.Raw(`SELECT token_max_limit, token_reset_duration, token_current_usage, token_last_reset,
		request_max_limit, request_reset_duration, request_current_usage, request_last_reset, soft_limit, config_hash
		FROM governance_rate_limits WHERE model_config_id = ? AND deleted = `+deletedLiteral(dialect, false), fromModelConfigID).Scan(&rateLimits).Error; err != nil {
		return err
	}
	for _, rl := range rateLimits {
		newID := uuid.NewString()
		if dialect == "postgres" {
			if err := db.Exec(`INSERT INTO governance_rate_limits (id, token_max_limit, token_reset_duration, token_current_usage, token_last_reset,
				request_max_limit, request_reset_duration, request_current_usage, request_last_reset, model_config_id, soft_limit, config_hash, created_at, updated_at, deleted)
				SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW(), false
				WHERE NOT EXISTS (
					SELECT 1 FROM governance_rate_limits
					WHERE model_config_id = ? AND deleted = false
					AND COALESCE(token_reset_duration, '') = COALESCE(?, '')
					AND COALESCE(request_reset_duration, '') = COALESCE(?, '')
				)`, newID, rl.TokenMaxLimit, rl.TokenResetDuration, rl.TokenCurrentUsage, rl.TokenLastReset,
				rl.RequestMaxLimit, rl.RequestResetDuration, rl.RequestCurrentUsage, rl.RequestLastReset,
				toModelConfigID, rl.SoftLimit, rl.ConfigHash, toModelConfigID, rl.TokenResetDuration, rl.RequestResetDuration).Error; err != nil {
				return err
			}
		} else {
			if err := db.Exec(`INSERT INTO governance_rate_limits (id, token_max_limit, token_reset_duration, token_current_usage, token_last_reset,
				request_max_limit, request_reset_duration, request_current_usage, request_last_reset, model_config_id, soft_limit, config_hash, created_at, updated_at, deleted)
				SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'), 0
				WHERE NOT EXISTS (
					SELECT 1 FROM governance_rate_limits
					WHERE model_config_id = ? AND deleted = 0
					AND COALESCE(token_reset_duration, '') = COALESCE(?, '')
					AND COALESCE(request_reset_duration, '') = COALESCE(?, '')
				)`, newID, rl.TokenMaxLimit, rl.TokenResetDuration, rl.TokenCurrentUsage, rl.TokenLastReset,
				rl.RequestMaxLimit, rl.RequestResetDuration, rl.RequestCurrentUsage, rl.RequestLastReset,
				toModelConfigID, rl.SoftLimit, rl.ConfigHash, toModelConfigID, rl.TokenResetDuration, rl.RequestResetDuration).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func repointSingleModelConfigID(db *gorm.DB, dialect, fromID, toID string) error {
	if err := db.Exec(`UPDATE governance_budgets SET model_config_id = ? WHERE model_config_id = ?`, toID, fromID).Error; err != nil {
		return err
	}
	return db.Exec(`UPDATE governance_rate_limits SET model_config_id = ? WHERE model_config_id = ?`, toID, fromID).Error
}

func deletedLiteral(dialect string, deleted bool) string {
	if dialect == "sqlite" {
		if deleted {
			return "1"
		}
		return "0"
	}
	if deleted {
		return "true"
	}
	return "false"
}
