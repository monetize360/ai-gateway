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

var configTables = []string{
	"config_providers",
	"config_keys",
	"config_models",
	"config_mcp_clients",
	"config_plugins",
	"config_client",
	"config_env_keys",
	"config_log_store",
	"config_vector_store",
}

func triggerConfigAuditMigrations(ctx context.Context, db *gorm.DB) error {
	if err := migrationConfigAddAuditColumns(ctx, db); err != nil {
		return err
	}
	if err := migrationConfigUUIDPKProviders(ctx, db); err != nil {
		return err
	}
	if err := migrationConfigUUIDPKKeysAndModels(ctx, db); err != nil {
		return err
	}
	if err := migrationConfigUUIDPKMCPPlugins(ctx, db); err != nil {
		return err
	}
	if err := migrationConfigUUIDPKSingletons(ctx, db); err != nil {
		return err
	}
	if err := migrationGovernanceConfigFKRealign(ctx, db); err != nil {
		return err
	}
	if err := migrationConfigSoftDeleteIndexes(ctx, db); err != nil {
		return err
	}
	return nil
}

func migrationConfigAddAuditColumns(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_add_audit_columns",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			mg := tx.Migrator()
			models := []any{
				&tables.TableProvider{},
				&tables.TableKey{},
				&tables.TableModel{},
				&tables.TableMCPClient{},
				&tables.TablePlugin{},
				&tables.TableClientConfig{},
				&tables.TableEnvKey{},
				&tables.TableLogStoreConfig{},
				&tables.TableVectorStoreConfig{},
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
				if !mg.HasColumn(model, "CreatedAt") {
					if err := mg.AddColumn(model, "CreatedAt"); err != nil {
						return fmt.Errorf("add CreatedAt: %w", err)
					}
				}
				if !mg.HasColumn(model, "UpdatedAt") {
					if err := mg.AddColumn(model, "UpdatedAt"); err != nil {
						return fmt.Errorf("add UpdatedAt: %w", err)
					}
				}
			}
			for _, table := range configTables {
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
		return fmt.Errorf("config_add_audit_columns: %w", err)
	}
	return nil
}

func migrationConfigUUIDPKProviders(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_uuid_pk_providers",
		Migrate: func(tx *gorm.DB) error {
			return migrateConfigProvidersUintToUUID(tx.WithContext(ctx))
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("config_uuid_pk_providers: %w", err)
	}
	return nil
}

func migrateConfigProvidersUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableProvider{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "config_providers", "id")
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
	if err := tx.Table("config_providers").Select("id").Find(&rows).Error; err != nil {
		return err
	}
	if err := tx.Exec("DROP TABLE IF EXISTS _config_provider_id_map").Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE TEMP TABLE _config_provider_id_map (old_id INTEGER PRIMARY KEY, new_id TEXT NOT NULL)`).Error; err != nil {
		return err
	}
	for _, row := range rows {
		newID := uuid.NewString()
		if err := tx.Exec("INSERT INTO _config_provider_id_map (old_id, new_id) VALUES (?, ?)", row.ID, newID).Error; err != nil {
			return err
		}
	}
	cols, err := tableColumnsExceptID(tx, "config_providers")
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	selectCols := make([]string, len(cols))
	for i, c := range cols {
		if c == "budget_id" || c == "rate_limit_id" {
			selectCols[i] = fmt.Sprintf("o.%s", c)
			continue
		}
		selectCols[i] = fmt.Sprintf("o.%s", c)
	}
	selectList := strings.Join(selectCols, ", ")
	return sqliteOrPostgresRebuildTable(tx, "config_providers", &tables.TableProvider{}, func(oldTable, newTable string) error {
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT m.new_id, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
			JOIN _config_provider_id_map m ON m.old_id = o.id
		`, quoteIdent(newTable), colList, selectList, quoteIdent(oldTable))).Error
	})
}

func migrationConfigUUIDPKKeysAndModels(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_uuid_pk_keys_and_models",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := migrateConfigKeysUintToUUID(tx); err != nil {
				return err
			}
			if err := migrateConfigModelsToUUID(tx); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("config_uuid_pk_keys_and_models: %w", err)
	}
	return nil
}

func migrateConfigKeysUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableKey{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "config_keys", "id")
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
	if err := tx.Table("config_keys").Select("id").Find(&rows).Error; err != nil {
		return err
	}
	if err := tx.Exec("DROP TABLE IF EXISTS _config_key_id_map").Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE TEMP TABLE _config_key_id_map (old_id INTEGER PRIMARY KEY, new_id TEXT NOT NULL)`).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := tx.Exec("INSERT INTO _config_key_id_map (old_id, new_id) VALUES (?, ?)", row.ID, uuid.NewString()).Error; err != nil {
			return err
		}
	}
	cols, err := tableColumnsExceptID(tx, "config_keys")
	if err != nil {
		return err
	}
	otherCols := make([]string, 0, len(cols))
	for _, c := range cols {
		if c != "provider_id" {
			otherCols = append(otherCols, c)
		}
	}
	colList := strings.Join(otherCols, ", ")
	providerJoin := ""
	providerSelect := "CAST(o.provider_id AS TEXT)"
	if tx.Migrator().HasTable("_config_provider_id_map") {
		providerJoin = "LEFT JOIN _config_provider_id_map pm ON pm.old_id = o.provider_id"
		providerSelect = "COALESCE(pm.new_id, CAST(o.provider_id AS TEXT))"
	}
	return sqliteOrPostgresRebuildTable(tx, "config_keys", &tables.TableKey{}, func(oldTable, newTable string) error {
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, provider_id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT km.new_id, %s, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
			JOIN _config_key_id_map km ON km.old_id = o.id
			%s
		`, quoteIdent(newTable), colList, providerSelect, colList, quoteIdent(oldTable), providerJoin)).Error
	})
}

func migrateConfigModelsToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableModel{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "config_models", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	providerJoin := ""
	providerSelect := "o.provider_id"
	if tx.Migrator().HasTable("_config_provider_id_map") {
		providerJoin = "LEFT JOIN _config_provider_id_map pm ON pm.old_id = o.provider_id"
		providerSelect = "COALESCE(pm.new_id, CAST(o.provider_id AS TEXT))"
	}
	return sqliteOrPostgresRebuildTable(tx, "config_models", &tables.TableModel{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, provider_id, name, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, %s, o.name, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
			%s
		`, quoteIdent(newTable), idExpr, providerSelect, quoteIdent(oldTable), providerJoin)).Error
	})
}

func migrationConfigUUIDPKMCPPlugins(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_uuid_pk_mcp_plugins",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := migrateConfigMCPClientsUintToUUID(tx); err != nil {
				return err
			}
			if err := migrateConfigPluginsUintToUUID(tx); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("config_uuid_pk_mcp_plugins: %w", err)
	}
	return nil
}

func migrateConfigMCPClientsUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableMCPClient{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "config_mcp_clients", "id")
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
	if err := tx.Table("config_mcp_clients").Select("id").Find(&rows).Error; err != nil {
		return err
	}
	if err := tx.Exec("DROP TABLE IF EXISTS _config_mcp_id_map").Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE TEMP TABLE _config_mcp_id_map (old_id INTEGER PRIMARY KEY, new_id TEXT NOT NULL)`).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := tx.Exec("INSERT INTO _config_mcp_id_map (old_id, new_id) VALUES (?, ?)", row.ID, uuid.NewString()).Error; err != nil {
			return err
		}
	}
	cols, err := tableColumnsExceptID(tx, "config_mcp_clients")
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	return sqliteOrPostgresRebuildTable(tx, "config_mcp_clients", &tables.TableMCPClient{}, func(oldTable, newTable string) error {
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT m.new_id, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
			JOIN _config_mcp_id_map m ON m.old_id = o.id
		`, quoteIdent(newTable), colList, colList, quoteIdent(oldTable))).Error
	})
}

func migrateConfigPluginsUintToUUID(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TablePlugin{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "config_plugins", "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	cols, err := tableColumnsExceptID(tx, "config_plugins")
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	return sqliteOrPostgresRebuildTable(tx, "config_plugins", &tables.TablePlugin{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), colList, idExpr, colList, quoteIdent(oldTable))).Error
	})
}

func migrationConfigUUIDPKSingletons(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_uuid_pk_singletons",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			singletons := []struct {
				table string
				model any
			}{
				{"config_client", &tables.TableClientConfig{}},
				{"config_log_store", &tables.TableLogStoreConfig{}},
				{"config_vector_store", &tables.TableVectorStoreConfig{}},
				{"config_env_keys", &tables.TableEnvKey{}},
			}
			for _, entry := range singletons {
				if err := migrateConfigSingletonUintToUUID(tx, entry.table, entry.model); err != nil {
					return err
				}
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("config_uuid_pk_singletons: %w", err)
	}
	return nil
}

func migrateConfigSingletonUintToUUID(tx *gorm.DB, table string, model any) error {
	if !tx.Migrator().HasTable(table) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, table, "id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	cols, err := tableColumnsExceptID(tx, table)
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	return sqliteOrPostgresRebuildTable(tx, table, model, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, %s, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, %s, o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), colList, idExpr, colList, quoteIdent(oldTable))).Error
	})
}

func migrationGovernanceConfigFKRealign(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "governance_config_fk_realign",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := realignJoinTableKeyIDs(tx); err != nil {
				return err
			}
			if err := realignVKMCPClientIDs(tx); err != nil {
				return err
			}
			return nil
		},
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("governance_config_fk_realign: %w", err)
	}
	return nil
}

func realignJoinTableKeyIDs(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableVirtualKeyProviderConfigKey{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_virtual_key_provider_config_keys", "table_key_id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	if tx.Migrator().HasTable("_config_key_id_map") {
		if err := tx.Exec(`
			UPDATE governance_virtual_key_provider_config_keys
			SET table_key_id = (
				SELECT m.new_id FROM _config_key_id_map m
				WHERE m.old_id = governance_virtual_key_provider_config_keys.table_key_id
			)
			WHERE EXISTS (
				SELECT 1 FROM _config_key_id_map m
				WHERE m.old_id = governance_virtual_key_provider_config_keys.table_key_id
			)
		`).Error; err != nil {
			return err
		}
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

func realignVKMCPClientIDs(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&tables.TableVirtualKeyMCPConfig{}) {
		return nil
	}
	isUUID, err := governancePKIsUUIDType(tx, "governance_virtual_key_mcp_configs", "mcp_client_id")
	if err != nil {
		return err
	}
	if isUUID {
		return nil
	}
	if tx.Migrator().HasTable("_config_mcp_id_map") {
		return tx.Exec(`
			UPDATE governance_virtual_key_mcp_configs
			SET mcp_client_id = (
				SELECT m.new_id FROM _config_mcp_id_map m
				WHERE m.old_id = governance_virtual_key_mcp_configs.mcp_client_id
			)
			WHERE EXISTS (
				SELECT 1 FROM _config_mcp_id_map m
				WHERE m.old_id = governance_virtual_key_mcp_configs.mcp_client_id
			)
		`).Error
	}
	return sqliteOrPostgresRebuildTable(tx, "governance_virtual_key_mcp_configs", &tables.TableVirtualKeyMCPConfig{}, func(oldTable, newTable string) error {
		idExpr := newUUIDSQLExpr(tx)
		mcpExpr := "CAST(o.mcp_client_id AS TEXT)"
		return tx.Exec(fmt.Sprintf(`
			INSERT INTO %s (id, virtual_key_id, mcp_client_id, tools_to_execute, created_by, updated_by, created_at, updated_at, deleted)
			SELECT %s, o.virtual_key_id, %s, o.tools_to_execute,
			       o.created_by, o.updated_by, COALESCE(o.created_at, CURRENT_TIMESTAMP), COALESCE(o.updated_at, CURRENT_TIMESTAMP), COALESCE(o.deleted, false)
			FROM %s o
		`, quoteIdent(newTable), idExpr, mcpExpr, quoteIdent(oldTable))).Error
	})
}

func migrationConfigSoftDeleteIndexes(ctx context.Context, db *gorm.DB) error {
	m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "config_soft_delete_indexes",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			indexes := []string{
				`CREATE INDEX IF NOT EXISTS idx_config_keys_provider_id ON config_keys (provider_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_config_keys_key_id ON config_keys (key_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_config_mcp_clients_client_id ON config_mcp_clients (client_id) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_config_providers_name ON config_providers (name) WHERE deleted = false`,
				`CREATE INDEX IF NOT EXISTS idx_config_models_provider_id ON config_models (provider_id) WHERE deleted = false`,
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
		return fmt.Errorf("config_soft_delete_indexes: %w", err)
	}
	return nil
}
