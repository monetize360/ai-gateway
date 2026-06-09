package configstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// PostgresConfig represents the configuration for a Postgres database.
type PostgresConfig struct {
	Host         *schemas.EnvVar `json:"host"`
	Port         *schemas.EnvVar `json:"port"`
	User         *schemas.EnvVar `json:"user"`
	Password     *schemas.EnvVar `json:"password"`
	DBName       *schemas.EnvVar `json:"db_name"`
	SSLMode      *schemas.EnvVar `json:"ssl_mode"`
	MaxIdleConns int             `json:"max_idle_conns"`
	MaxOpenConns int             `json:"max_open_conns"`
}

// buildPostgresDSN assembles a libpq-style DSN from the validated config.
func buildPostgresDSN(config *PostgresConfig) string {
	return ensurePostgresDSNUTC(fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		config.Host.GetValue(), config.Port.GetValue(), config.User.GetValue(),
		config.Password.GetValue(), config.DBName.GetValue(), config.SSLMode.GetValue()))
}

// ensurePostgresDSNUTC appends timezone=UTC when missing so timestamp comparisons
// match MPilot rows stored as UTC wall time in timestamp without time zone columns.
func ensurePostgresDSNUTC(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return dsn
	}
	if strings.Contains(strings.ToLower(dsn), "timezone=") {
		return dsn
	}
	return dsn + " timezone=UTC"
}

// openPostresConnection opens a *gorm.DB against the configured Postgres instance.
func openPostresConnection(dsn string, logger schemas.Logger) (*gorm.DB, error) {
	return gorm.Open(postgres.New(postgres.Config{DSN: ensurePostgresDSNUTC(dsn)}), &gorm.Config{
		Logger: newGormLogger(logger),
	})
}

// closeDbConn closes the *sql.DB backing a *gorm.DB, logging any error.
func closeDbConn(db *gorm.DB, logger schemas.Logger) {
	sqlDB, err := db.DB()
	if err != nil {
		logger.Error("failed to resolve *sql.DB for close: %v", err)
		return
	}
	if err := sqlDB.Close(); err != nil {
		logger.Error("failed to close DB connection: %v", err)
	}
}

// PostgresPoolSettings holds sql.DB pool limits for Postgres connections.
// Zero values use package defaults (5 idle, 50 open).
type PostgresPoolSettings struct {
	MaxIdleConns int
	MaxOpenConns int
}

func (p PostgresPoolSettings) apply(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	maxIdleConns := p.MaxIdleConns
	if maxIdleConns == 0 {
		maxIdleConns = 5
	}
	sqlDB.SetMaxIdleConns(maxIdleConns)
	maxOpenConns := p.MaxOpenConns
	if maxOpenConns == 0 {
		maxOpenConns = 50
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	return nil
}

// applyPostgresPoolTuning applies MaxIdleConns / MaxOpenConns from config to
// the supplied *gorm.DB, falling back to defaults when the config leaves the
// field at zero.
func applyPostgresPoolTuning(db *gorm.DB, config *PostgresConfig) error {
	return PostgresPoolSettings{
		MaxIdleConns: config.MaxIdleConns,
		MaxOpenConns: config.MaxOpenConns,
	}.apply(db)
}

// NewPostgresConfigStoreFromDSN creates a Postgres ConfigStore from a pre-built
// connection string. Schema management is the caller's responsibility; Bifrost
// only opens a runtime pool and reads/writes existing tables.
func NewPostgresConfigStoreFromDSN(ctx context.Context, dsn string, pool PostgresPoolSettings, logger schemas.Logger) (ConfigStore, error) {
	db, err := openPostresConnection(dsn, logger)
	if err != nil {
		return nil, err
	}
	if err := pool.apply(db); err != nil {
		closeDbConn(db, logger)
		return nil, fmt.Errorf("failed to tune tenant DB pool: %w", err)
	}

	d := &RDBConfigStore{logger: logger}
	d.db.Store(db)

	d.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, d.DB())
	}
	d.refreshPoolFn = func(ctx context.Context) error {
		newDB, err := openPostresConnection(dsn, logger)
		if err != nil {
			return fmt.Errorf("failed to open fresh runtime pool: %w", err)
		}
		if err := pool.apply(newDB); err != nil {
			closeDbConn(newDB, logger)
			return fmt.Errorf("failed to tune fresh runtime pool: %w", err)
		}
		oldDB := d.db.Swap(newDB)
		if oldDB != nil {
			closeDbConn(oldDB, logger)
		}
		return nil
	}

	if err := d.EncryptPlaintextRows(ctx); err != nil {
		closeDbConn(db, logger)
		return nil, fmt.Errorf("failed to encrypt plaintext rows: %w", err)
	}
	return d, nil
}

// newPostgresConfigStore creates a new Postgres config store.
func newPostgresConfigStore(ctx context.Context, config *PostgresConfig, logger schemas.Logger) (ConfigStore, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	if config.Host == nil || config.Host.GetValue() == "" {
		return nil, fmt.Errorf("postgres host is required")
	}
	if config.Port == nil || config.Port.GetValue() == "" {
		return nil, fmt.Errorf("postgres port is required")
	}
	if config.User == nil || config.User.GetValue() == "" {
		return nil, fmt.Errorf("postgres user is required")
	}
	if config.Password == nil {
		return nil, fmt.Errorf("postgres password is required")
	}
	if config.DBName == nil || config.DBName.GetValue() == "" {
		return nil, fmt.Errorf("postgres db name is required")
	}
	if config.SSLMode == nil || config.SSLMode.GetValue() == "" {
		return nil, fmt.Errorf("postgres ssl mode is required")
	}
	dsn := buildPostgresDSN(config)

	// Runtime pool.
	db, err := openPostresConnection(dsn, logger)
	if err != nil {
		return nil, err
	}
	if err := applyPostgresPoolTuning(db, config); err != nil {
		closeDbConn(db, logger)
		return nil, err
	}

	if err := autoMigrateConfigTables(db); err != nil {
		closeDbConn(db, logger)
		return nil, fmt.Errorf("failed to auto-migrate configstore tables: %w", err)
	}

	d := &RDBConfigStore{logger: logger}
	d.db.Store(db)

	d.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, d.DB())
	}

	// refreshPoolFn: open fresh runtime pool first (so a failure leaves the
	// existing pool in place), swap atomically, then close the old pool.
	// sql.DB.Close blocks until in-flight queries finish, so callers already
	// using the old pool complete safely.
	d.refreshPoolFn = func(ctx context.Context) error {
		newDB, err := openPostresConnection(dsn, logger)
		if err != nil {
			return fmt.Errorf("failed to open fresh runtime pool: %w", err)
		}
		if err := applyPostgresPoolTuning(newDB, config); err != nil {
			closeDbConn(newDB, logger)
			return fmt.Errorf("failed to tune fresh runtime pool: %w", err)
		}
		oldDB := d.db.Swap(newDB)
		if oldDB != nil {
			closeDbConn(oldDB, logger)
		}
		return nil
	}

	// Encrypt any plaintext rows if encryption is enabled. Runs on the
	// runtime pool — pure DML (SELECT + UPDATE), no DDL, so cached plans it
	// installs remain valid until the next external migration batch.
	if err := d.EncryptPlaintextRows(ctx); err != nil {
		closeDbConn(db, logger)
		return nil, fmt.Errorf("failed to encrypt plaintext rows: %w", err)
	}
	return d, nil
}
