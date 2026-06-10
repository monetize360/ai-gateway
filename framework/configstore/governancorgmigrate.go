package configstore

import (
	"fmt"

	"gorm.io/gorm"
)

// migrateGovernanceOrgOwnership copies org-limit rows into governed_organization_id
// on budget/rate-limit child rows, then drops governance_org_limits.
func migrateGovernanceOrgOwnership(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable("governance_org_limits") {
		return nil
	}
	dialect := db.Dialector.Name()

	migrations := []struct {
		name string
		sql  string
	}{
		{
			name: "budgets.governed_organization_id from governance_org_limits",
			sql:  orgOwnershipFromOrgLimitsSQL(dialect, "governance_budgets", "budget_id"),
		},
		{
			name: "rate_limits.governed_organization_id from governance_org_limits",
			sql:  orgOwnershipFromOrgLimitsSQL(dialect, "governance_rate_limits", "rate_limit_id"),
		},
	}
	for _, m := range migrations {
		if m.sql == "" {
			continue
		}
		if err := db.Exec(m.sql).Error; err != nil {
			return fmt.Errorf("governance org ownership migration %s: %w", m.name, err)
		}
	}

	if err := db.Exec("DROP TABLE IF EXISTS governance_org_limits").Error; err != nil {
		return fmt.Errorf("drop governance_org_limits: %w", err)
	}
	return nil
}

func orgOwnershipFromOrgLimitsSQL(dialect, childTable, refCol string) string {
	switch dialect {
	case "postgres":
		return fmt.Sprintf(
			`UPDATE %s AS c SET governed_organization_id = ol.org_id FROM governance_org_limits AS ol WHERE ol.%s = c.id AND c.governed_organization_id IS NULL AND ol.%s IS NOT NULL AND ol.deleted = false`,
			childTable, refCol, refCol,
		)
	case "sqlite":
		return fmt.Sprintf(
			`UPDATE %s SET governed_organization_id = (SELECT ol.org_id FROM governance_org_limits ol WHERE ol.%s = %s.id AND ol.deleted = 0 LIMIT 1) WHERE governed_organization_id IS NULL AND EXISTS (SELECT 1 FROM governance_org_limits ol WHERE ol.%s = %s.id AND ol.%s IS NOT NULL AND ol.deleted = 0)`,
			childTable, refCol, childTable, refCol, childTable, refCol,
		)
	default:
		return ""
	}
}
