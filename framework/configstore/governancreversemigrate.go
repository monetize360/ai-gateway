package configstore

import (
	"fmt"

	"gorm.io/gorm"
)

// migrateGovernanceReverseOwnership copies legacy parent FK columns into child-row
// ownership columns (provider_id, model_config_id, virtual_key_id on budgets/rate_limits).
func migrateGovernanceReverseOwnership(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	dialect := db.Dialector.Name()

	migrations := []struct {
		name string
		sql  string
	}{
		{
			name: "budgets.provider_id from config_providers.budget_id",
			sql:  reverseOwnershipSQL(dialect, "governance_budgets", "provider_id", "config_providers", "budget_id"),
		},
		{
			name: "rate_limits.provider_id from config_providers.rate_limit_id",
			sql:  reverseOwnershipSQL(dialect, "governance_rate_limits", "provider_id", "config_providers", "rate_limit_id"),
		},
		{
			name: "budgets.model_config_id from governance_model_configs.budget_id",
			sql:  reverseOwnershipSQL(dialect, "governance_budgets", "model_config_id", "governance_model_configs", "budget_id"),
		},
		{
			name: "rate_limits.model_config_id from governance_model_configs.rate_limit_id",
			sql:  reverseOwnershipSQL(dialect, "governance_rate_limits", "model_config_id", "governance_model_configs", "rate_limit_id"),
		},
		{
			name: "rate_limits.virtual_key_id from governance_virtual_keys.rate_limit_id",
			sql:  reverseOwnershipSQL(dialect, "governance_rate_limits", "virtual_key_id", "governance_virtual_keys", "rate_limit_id"),
		},
	}

	for _, m := range migrations {
		if m.sql == "" {
			continue
		}
		if !db.Migrator().HasTable(tableFromReverseOwnershipSQL(m.sql)) {
			continue
		}
		if err := db.Exec(m.sql).Error; err != nil {
			return fmt.Errorf("governance reverse ownership migration %s: %w", m.name, err)
		}
	}
	return nil
}

func reverseOwnershipSQL(dialect, childTable, childOwnerCol, parentTable, parentRefCol string) string {
	switch dialect {
	case "postgres":
		return fmt.Sprintf(
			`UPDATE %s AS c SET %s = p.id FROM %s AS p WHERE p.%s = c.id AND c.%s IS NULL AND p.%s IS NOT NULL AND p.deleted = false`,
			childTable, childOwnerCol, parentTable, parentRefCol, childOwnerCol, parentRefCol,
		)
	case "sqlite":
		return fmt.Sprintf(
			`UPDATE %s SET %s = (SELECT p.id FROM %s p WHERE p.%s = %s.id AND p.deleted = 0 LIMIT 1) WHERE %s IS NULL AND EXISTS (SELECT 1 FROM %s p WHERE p.%s = %s.id AND p.%s IS NOT NULL AND p.deleted = 0)`,
			childTable, childOwnerCol, parentTable, parentRefCol, childTable, childOwnerCol, parentTable, parentRefCol, childTable, parentRefCol,
		)
	default:
		return ""
	}
}

func tableFromReverseOwnershipSQL(sql string) string {
	// UPDATE <table> ...
	if len(sql) < 7 {
		return ""
	}
	rest := sql[7:]
	for i, ch := range rest {
		if ch == ' ' {
			return rest[:i]
		}
	}
	return rest
}
