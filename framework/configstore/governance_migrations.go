package configstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

var governanceTables = []string{
	"governance_rate_limits",
	"governance_budgets",
	"governance_customers",
	"governance_teams",
	"governance_virtual_keys",
	"governance_virtual_key_provider_configs",
	"governance_virtual_key_mcp_configs",
	"governance_virtual_key_provider_config_keys",
	"governance_model_configs",
	"governance_pricing_overrides",
	"governance_model_pricing",
	"governance_model_parameters",
	"governance_config",
}

func triggerGovernanceAuditMigrations(ctx context.Context, db *gorm.DB) error {
	if err := migrationGovernanceAddAuditColumns(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceVKCreatedByRename(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceChildUUIDPKs(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceTopLevelUUIDColumns(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceConfigRekey(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceSoftDeleteIndexes(ctx, db); err != nil {
		return err
	}
	return nil
}

func migrationGovernanceAddAuditColumns(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_add_audit_columns",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			models := []any{
				&tables.TableRateLimit{},
				&tables.TableBudget{},
				&tables.TableCustomer{},
				&tables.TableTeam{},
				&tables.TableVirtualKey{},
				&tables.TableVirtualKeyProviderConfig{},
				&tables.TableVirtualKeyMCPConfig{},
				&tables.TableVirtualKeyProviderConfigKey{},
				&tables.TableModelConfig{},
				&tables.TablePricingOverride{},
				&tables.TableModelPricing{},
				&tables.TableModelParameters{},
				&tables.TableGovernanceConfig{},
			}
			for _, model := range models {
				if !mg.HasTable(model) {
					continue
				}
				for _, col := range []string{"CreatedBy", "UpdatedBy", "Deleted"} {
					if !mg.HasColumn(model, col) {
						if err := mg.AddColumn(model, col); err != nil {
							return fmt.Errorf("add %s audit column: %w", col, err)
						}
					}
				}
			}
			for _, table := range governanceTables {
				if tx.Migrator().HasTable(table) {
					if err := tx.Exec(fmt.Sprintf("UPDATE %s SET deleted = false WHERE deleted IS NULL", quoteIdent(table))).Error; err != nil {
						return fmt.Errorf("backfill deleted on %s: %w", table, err)
					}
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_add_audit_columns: %w", err)
	}
	return nil
}

func migrationGovernanceVKCreatedByRename(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_vk_created_by_rename",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasTable(&tables.TableVirtualKey{}) {
				return nil
			}
			hasLegacy, err := hasColumn(tx, "governance_virtual_keys", "created_by_user_id")
			if err != nil {
				return err
			}
			if !hasLegacy {
				return nil
			}
			hasNew, err := hasColumn(tx, "governance_virtual_keys", "created_by")
			if err != nil {
				return err
			}
			if !hasNew {
				if err := tx.Migrator().AddColumn(&tables.TableVirtualKey{}, "CreatedBy"); err != nil {
					return fmt.Errorf("add created_by: %w", err)
				}
			}
			if err := tx.Exec(`
				UPDATE governance_virtual_keys
				SET created_by = created_by_user_id
				WHERE created_by IS NULL AND created_by_user_id IS NOT NULL AND created_by_user_id <> ''
			`).Error; err != nil {
				return fmt.Errorf("copy created_by_user_id: %w", err)
			}
			if tx.Dialector.Name() == "sqlite" {
				if err := tx.Exec("ALTER TABLE governance_virtual_keys DROP COLUMN created_by_user_id").Error; err != nil {
					return sqliteDropColumn(tx, "governance_virtual_keys", "created_by_user_id", &tables.TableVirtualKey{})
				}
				return nil
			}
			return tx.Exec("ALTER TABLE governance_virtual_keys DROP COLUMN IF EXISTS created_by_user_id").Error
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_vk_created_by_rename: %w", err)
	}
	return nil
}

func migrationGovernanceChildUUIDPKs(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_uuid_pk_children",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := migrateProviderConfigUintToUUID(tx); err != nil {
				return err
			}
			if err := migrateVKMCPConfigUintToUUID(tx); err != nil {
				return err
			}
			if err := migrateProviderConfigKeysJoinTable(tx); err != nil {
				return err
			}
			if err := migrateModelPricingUintToUUID(tx); err != nil {
				return err
			}
			if err := migrateModelParametersUintToUUID(tx); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_uuid_pk_children: %w", err)
	}
	return nil
}

func migrateProviderConfigUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableVirtualKeyProviderConfig{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_virtual_key_provider_configs", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	type oldRow struct {
		ID uint
	}
	var rows []oldRow
	if err := tx.Table("governance_virtual_key_provider_configs").Select("id").Find(&rows).Error; err != nil {
		return err
	}
	idMap := make(map[uint]string, len(rows))
	for _, row := range rows {
		idMap[row.ID] = uuid.NewString()
	}
	if err := tx.Exec("DROP TABLE IF EXISTS _governance_pc_id_map").Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE TEMP TABLE _governance_pc_id_map (old_id INTEGER PRIMARY KEY, new_id TEXT NOT NULL)`).Error; err != nil {
		return err
	}
	for oldID, newID := range idMap {
		if err := tx.Exec("INSERT INTO _governance_pc_id_map (old_id, new_id) VALUES (?, ?)", oldID, newID).Error; err != nil {
			return err
		}
	}
	if tx.Migrator().HasColumn(&tables.TableBudget{}, "provider_config_id") {
		for oldID, newID := range idMap {
			if err := tx.Exec("UPDATE governance_budgets SET provider_config_id = ? WHERE provider_config_id = ?", newID, oldID).Error; err != nil {
				return err
			}
		}
	}
	return sqliteOrPostgresRebuildTable(tx, "governance_virtual_key_provider_configs", &tables.TableVirtualKeyProviderConfig{}, func(oldTable, newTable string) error {
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, virtual_key_id, provider, weight, allowed_models, blacklisted_models, allow_all_keys, rate_limit_id, created_by, updated_by, created_at, updated_at, deleted)
			SELECT m.new_id, o.virtual_key_id, o.provider, o.weight, o.allowed_models, o.blacklisted_models, o.allow_all_keys, o.rate_limit_id,
			       o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
			JOIN _governance_pc_id_map m ON m.old_id = o.id
		`, quoteIdent(newTable), quoteIdent(oldTable))).Error
	})
}

func migrateVKMCPConfigUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableVirtualKeyMCPConfig{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_virtual_key_mcp_configs", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	return sqliteOrPostgresRebuildTable(tx, "governance_virtual_key_mcp_configs", &tables.TableVirtualKeyMCPConfig{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, virtual_key_id, mcp_client_id, tools_to_execute, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, o.virtual_key_id, o.mcp_client_id, o.tools_to_execute,
			       o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), idExpr, quoteIdent(oldTable))).Error
	})
}

func migrateProviderConfigKeysJoinTable(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableVirtualKeyProviderConfigKey{}) {
		return nil
	}
	hasID, err := hasColumn(tx, "governance_virtual_key_provider_config_keys", "id")
	if err != nil {
		return err
	}
	if hasID {
		isUUID, err := governancePKIsUUIDType(tx, "governance_virtual_key_provider_config_keys", "id")
		if err != nil {
			return err
		}
		if isUUID {
			return nil
		}
	}
	oldHasUintPC, _ := hasColumn(tx, "governance_virtual_key_provider_config_keys", "table_virtual_key_provider_config_id")
	if !oldHasUintPC {
		return tx.Migrator().AutoMigrate(&tables.TableVirtualKeyProviderConfigKey{})
	}
	return sqliteOrPostgresRebuildTable(tx, "governance_virtual_key_provider_config_keys", &tables.TableVirtualKeyProviderConfigKey{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, table_virtual_key_provider_config_id, table_key_id, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, o.table_virtual_key_provider_config_id, o.table_key_id,
			       o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), idExpr, quoteIdent(oldTable))).Error
	})
}

func migrateModelPricingUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableModelPricing{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_model_pricing", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	cols, err := tableColumnsExceptID(tx, "governance_model_pricing")
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	return sqliteOrPostgresRebuildTable(tx, "governance_model_pricing", &tables.TableModelPricing{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), colList, idExpr, colList, quoteIdent(oldTable))).Error
	})
}

func migrateModelParametersUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableModelParameters{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_model_parameters", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	return sqliteOrPostgresRebuildTable(tx, "governance_model_parameters", &tables.TableModelParameters{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, model, data, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, o.model, o.data, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), idExpr, quoteIdent(oldTable))).Error
	})
}

func migrationGovernanceTopLevelUUIDColumns(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_uuid_pk_top_level",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			topLevel := []struct {
				table string
				model any
			}{
				{"governance_rate_limits", &tables.TableRateLimit{}},
				{"governance_budgets", &tables.TableBudget{}},
				{"governance_customers", &tables.TableCustomer{}},
				{"governance_teams", &tables.TableTeam{}},
				{"governance_virtual_keys", &tables.TableVirtualKey{}},
				{"governance_model_configs", &tables.TableModelConfig{}},
				{"governance_pricing_overrides", &tables.TablePricingOverride{}},
			}
			for _, entry := range topLevel {
				if !tx.Migrator().HasTable(entry.table) {
					continue
				}
				isUUID, err := governancePKIsUUIDType(tx, entry.table, "id")
				if err != nil {
					return err
				}
				if isUUID {
					continue
				}
				if err := normalizeGovernanceStringIDs(tx, entry.table); err != nil {
					return err
				}
				if tx.Dialector.Name() == "postgres" {
					if err := tx.Exec(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN id TYPE uuid USING id::uuid", quoteIdent(entry.table))).Error; err != nil {
						return fmt.Errorf("alter %s.id to uuid: %w", entry.table, err)
					}
				}
			}
			fkTables := []struct{ table, column string }{
				{"governance_budgets", "team_id"},
				{"governance_budgets", "virtual_key_id"},
				{"governance_budgets", "provider_config_id"},
				{"governance_customers", "budget_id"},
				{"governance_customers", "rate_limit_id"},
				{"governance_teams", "customer_id"},
				{"governance_teams", "rate_limit_id"},
				{"governance_virtual_keys", "team_id"},
				{"governance_virtual_keys", "customer_id"},
				{"governance_virtual_keys", "rate_limit_id"},
				{"governance_virtual_keys", "created_by"},
				{"governance_virtual_keys", "updated_by"},
				{"governance_virtual_key_provider_configs", "virtual_key_id"},
				{"governance_virtual_key_provider_configs", "rate_limit_id"},
				{"governance_virtual_key_mcp_configs", "virtual_key_id"},
				{"governance_virtual_key_provider_config_keys", "table_virtual_key_provider_config_id"},
				{"governance_model_configs", "budget_id"},
				{"governance_model_configs", "rate_limit_id"},
				{"governance_pricing_overrides", "virtual_key_id"},
			}
			for _, fk := range fkTables {
				if !tx.Migrator().HasTable(fk.table) {
					continue
				}
				exists, err := hasColumn(tx, fk.table, fk.column)
				if err != nil || !exists {
					continue
				}
				if tx.Dialector.Name() == "postgres" {
					_ = tx.Exec(fmt.Sprintf(
						"ALTER TABLE %s ALTER COLUMN %s TYPE uuid USING NULLIF(%s, '')::uuid",
						quoteIdent(fk.table), quoteIdent(fk.column), quoteIdent(fk.column),
					))
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_uuid_pk_top_level: %w", err)
	}
	return nil
}

func migrationGovernanceConfigRekey(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_config_rekey",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if !tx.Migrator().HasTable(&tables.TableGovernanceConfig{}) {
				return nil
			}
			hasConfigKey, err := hasColumn(tx, "governance_config", "config_key")
			if err != nil {
				return err
			}
			if hasConfigKey {
				return nil
			}
			hasLegacyKey, err := hasColumn(tx, "governance_config", "key")
			if err != nil {
				return err
			}
			if !hasLegacyKey {
				return tx.Migrator().AutoMigrate(&tables.TableGovernanceConfig{})
			}
			if tx.Dialector.Name() == "sqlite" {
				return sqliteRebuildGovernanceConfig(tx)
			}
			if err := tx.Exec("ALTER TABLE governance_config RENAME COLUMN key TO config_key").Error; err != nil {
				return err
			}
			if !tx.Migrator().HasColumn(&tables.TableGovernanceConfig{}, "ID") {
				if err := tx.Migrator().AddColumn(&tables.TableGovernanceConfig{}, "ID"); err != nil {
					return err
				}
			}
			return tx.Exec(`
				UPDATE governance_config SET id = gen_random_uuid()::text WHERE id IS NULL OR id = '';
				ALTER TABLE governance_config ALTER COLUMN id SET NOT NULL;
				ALTER TABLE governance_config DROP CONSTRAINT IF EXISTS governance_config_pkey;
				ALTER TABLE governance_config ADD PRIMARY KEY (id);
				CREATE UNIQUE INDEX IF NOT EXISTS idx_governance_config_config_key ON governance_config (config_key) WHERE deleted = false;
			`).Error
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_config_rekey: %w", err)
	}
	return nil
}

func migrationGovernanceSoftDeleteIndexes(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_soft_delete_indexes",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			indexes := []string{
				`CREATE INDEX IF NOT EXISTS idx_governance_virtual_keys_active ON governance_virtual_keys (team_id, customer_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_governance_budgets_virtual_key_id ON governance_budgets (virtual_key_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_governance_teams_customer_id ON governance_teams (customer_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_governance_vk_provider_configs_vk ON governance_virtual_key_provider_configs (virtual_key_id) WHERE deleted = false`,
			}
			for _, stmt := range indexes {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_soft_delete_indexes: %w", err)
	}
	return nil
}

func governancePKIsUUIDType(tx *gorm.DB, table, column string) (bool, error) {
	switch tx.Dialector.Name() {
	case "postgres":
		var dataType string
		err := tx.Raw(`
			SELECT data_type FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?
		`, table, column).Scan(&dataType).Error
		if err != nil {
			return false, err
		}
		return dataType == "uuid", nil
	default:
		columns, err := sqliteTableColumns(tx, table)
		if err != nil {
			return false, err
		}
		for _, col := range columns {
			if col == column {
				var row struct{ Type string }
				q := fmt.Sprintf("SELECT type FROM pragma_table_info(%s) WHERE name = ?", quoteSQLiteIdentifier(table))
				if err := tx.Raw(q, column).Scan(&row).Error; err != nil {
					return false, err
				}
				return !strings.EqualFold(row.Type, "INTEGER"), nil
			}
		}
		return false, nil
	}
}

func normalizeGovernanceStringIDs(tx *gorm.DB, table string) error {
	type idRow struct{ ID string }
	var rows []idRow
	if err := tx.Table(table).Select("id").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := uuid.Parse(row.ID); err == nil {
			continue
		}
		newID := uuid.NewString()
		if err := tx.Exec(fmt.Sprintf("UPDATE %s SET id = ? WHERE id = ?", quoteIdent(table)), newID, row.ID).Error; err != nil {
			return err
		}
		if err := rewriteGovernanceFKReferences(tx, row.ID, newID); err != nil {
			return err
		}
	}
	return nil
}

func rewriteGovernanceFKReferences(tx *gorm.DB, oldID, newID string) error {
	refs := map[string][]string{
		"governance_budgets":                      {"team_id", "virtual_key_id"},
		"governance_customers":                  {"budget_id", "rate_limit_id"},
		"governance_teams":                      {"customer_id", "rate_limit_id"},
		"governance_virtual_keys":               {"team_id", "customer_id", "rate_limit_id"},
		"governance_virtual_key_provider_configs": {"virtual_key_id", "rate_limit_id"},
		"governance_virtual_key_mcp_configs":    {"virtual_key_id"},
		"governance_model_configs":              {"budget_id", "rate_limit_id"},
		"governance_pricing_overrides":          {"virtual_key_id"},
		"config_providers":                      {"budget_id", "rate_limit_id"},
	}
	for table, cols := range refs {
		if !tx.Migrator().HasTable(table) {
			continue
		}
		for _, col := range cols {
			exists, err := hasColumn(tx, table, col)
			if err != nil || !exists {
				continue
			}
			if err := tx.Exec(fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", quoteIdent(table), quoteIdent(col), quoteIdent(col)), newID, oldID).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func newUUIDSQLExpr(tx *gorm.DB) string {
	if tx.Dialector.Name() == "postgres" {
		return "gen_random_uuid()::text"
	}
	return `lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6)))`
}

func sqliteOrPostgresRebuildTable(tx *gorm.DB, tableName string, model any, copyFn func(oldTable, newTable string) error) error {
	newTable := tableName + "__new"
	if err := tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteIdent(newTable))).Error; err != nil {
		return err
	}
	if err := tx.Table(newTable).Migrator().CreateTable(model); err != nil {
		return err
	}
	if err := copyFn(tableName, newTable); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf("DROP TABLE %s", quoteIdent(tableName))).Error; err != nil {
		return err
	}
	if err := tx.Migrator().RenameTable(newTable, tableName); err != nil {
		return err
	}
	return nil
}

func tableColumnsExceptID(tx *gorm.DB, table string) ([]string, error) {
	if tx.Dialector.Name() == "postgres" {
		var cols []string
		err := tx.Raw(`
			SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ?
			  AND column_name NOT IN ('id', 'created_by', 'updated_by', 'created_at', 'updated_at', 'deleted')
			ORDER BY ordinal_position
		`, table).Scan(&cols).Error
		return cols, err
	}
	all, err := sqliteTableColumns(tx, table)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{"id": true, "created_by": true, "updated_by": true, "created_at": true, "updated_at": true, "deleted": true}
	out := make([]string, 0, len(all))
	for _, c := range all {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

func sqliteDropColumn(tx *gorm.DB, tableName, column string, model any) error {
	columns, err := sqliteTableColumns(tx, tableName)
	if err != nil {
		return err
	}
	preserved := make([]string, 0, len(columns))
	for _, c := range columns {
		if c != column {
			preserved = append(preserved, c)
		}
	}
	return sqliteOrPostgresRebuildTable(tx, tableName, model, func(oldTable, newTable string) error {
		quoted := make([]string, len(preserved))
		for i, c := range preserved {
			quoted[i] = quoteSQLiteIdentifier(c)
		}
		colList := strings.Join(quoted, ", ")
		return tx.Exec(fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", quoteIdent(newTable), colList, colList, quoteIdent(oldTable))).Error
	})
}

func sqliteRebuildGovernanceConfig(tx *gorm.DB) error {
	type legacy struct {
		Key   string `gorm:"column:key"`
		Value string
	}
	var rows []legacy
	if err := tx.Table("governance_config").Find(&rows).Error; err != nil {
		return err
	}
	if err := tx.Migrator().DropTable(&tables.TableGovernanceConfig{}); err != nil {
		return err
	}
	if err := tx.Migrator().CreateTable(&tables.TableGovernanceConfig{}); err != nil {
		return err
	}
	for _, row := range rows {
		cfg := tables.TableGovernanceConfig{
			ID:        uuid.NewString(),
			ConfigKey: row.Key,
			Value:     row.Value,
		}
		if err := tx.Create(&cfg).Error; err != nil {
			return err
		}
	}
	return nil
}
