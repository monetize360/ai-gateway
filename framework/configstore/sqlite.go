package configstore

import (
	"context"
	"fmt"
	"os"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// SQLiteConfig represents the configuration for a SQLite database.
type SQLiteConfig struct {
	Path string `json:"path"`
}

// newSqliteConfigStore creates a new SQLite config store.
func newSqliteConfigStore(ctx context.Context, config *SQLiteConfig, logger schemas.Logger) (ConfigStore, error) {
	if _, err := os.Stat(config.Path); os.IsNotExist(err) {
		// Create DB file
		f, err := os.Create(config.Path)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=10000&_busy_timeout=60000&_wal_autocheckpoint=1000&_foreign_keys=1", config.Path)
	logger.Debug("opening DB with dsn: %s", dsn)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: newGormLogger(logger),
	})
	if err != nil {
		return nil, err
	}
	logger.Debug("db opened for configstore")
	s := &RDBConfigStore{logger: logger}
	s.db.Store(db)
	s.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, s.DB())
	}
	s.refreshPoolFn = func(ctx context.Context) error { return nil }

	if err := autoMigrateConfigTables(db); err != nil {
		return nil, fmt.Errorf("failed to auto-migrate configstore tables: %w", err)
	}
	// Encrypt any plaintext rows if encryption is enabled
	if err := s.EncryptPlaintextRows(ctx); err != nil {
		return nil, fmt.Errorf("failed to encrypt plaintext rows: %w", err)
	}
	return s, nil
}

// autoMigrateConfigTables runs GORM AutoMigrate for all configstore table models.
// AutoMigrate is idempotent: it creates missing tables/columns but never drops columns.
func autoMigrateConfigTables(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TableProvider{},
		&tables.TableKey{},
		&tables.TableModel{},
		&tables.TableOauthConfig{},
		&tables.TableOauthToken{},
		&tables.TableOauthUserSession{},
		&tables.TableOauthUserToken{},
		&tables.TableMCPClient{},
		&tables.TableClientConfig{},
		&tables.TableEnvKey{},
		&tables.TableVectorStoreConfig{},
		&tables.TableLogStoreConfig{},
		&tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableVirtualKeyMCPConfig{},
		&tables.TableVirtualKeyProviderConfigKey{},
		&tables.TableGovernanceConfig{},
		&tables.TableModelConfig{},
		&tables.TablePricingOverride{},
		&tables.TablePlugin{},
		&tables.TableFeatureFlag{},
		&tables.TableFrameworkConfig{},
		&tables.TableDistributedLock{},
		&tables.SessionsTable{},
		&tables.TempToken{},
		&tables.TableRoutingRule{},
		&tables.TableRoutingTarget{},
		&tables.TableFolder{},
		&tables.TablePrompt{},
		&tables.TablePromptVersion{},
		&tables.TablePromptVersionMessage{},
		&tables.TablePromptSession{},
		&tables.TablePromptSessionMessage{},
		&tables.TableAccessKeyToken{},
	); err != nil {
		return err
	}
	if err := migrateGovernanceReverseOwnership(db); err != nil {
		return err
	}
	return migrateGovernanceOrgOwnership(db)
}
