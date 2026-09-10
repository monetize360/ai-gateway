package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// PostgresConfig represents the configuration for a Postgres database.
type PostgresConfig struct {
	Host            *schemas.EnvVar `json:"host"`
	Port            *schemas.EnvVar `json:"port"`
	User            *schemas.EnvVar `json:"user"`
	Password        *schemas.EnvVar `json:"password"`
	DBName          *schemas.EnvVar `json:"db_name"`
	SSLMode         *schemas.EnvVar `json:"ssl_mode"`
	MaxIdleConns    int             `json:"max_idle_conns"`
	MaxOpenConns    int             `json:"max_open_conns"`
	ConnMaxIdleTime time.Duration   `json:"-"`
	ConnMaxLifetime time.Duration   `json:"-"`
	// MatViewRefreshInterval is retained for config compatibility only. Materialized
	// views are no longer created or refreshed by Bifrost at startup.
	MatViewRefreshInterval string `json:"matview_refresh_interval,omitempty"`
}

// PoolSettings tunes sql.DB limits for a log store connection pool.
type PoolSettings struct {
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}

func applyLogStorePool(sqlDB *sql.DB, pool PoolSettings) {
	maxIdleConns := pool.MaxIdleConns
	if maxIdleConns == 0 {
		maxIdleConns = 5
	}
	sqlDB.SetMaxIdleConns(maxIdleConns)
	maxOpenConns := pool.MaxOpenConns
	if maxOpenConns == 0 {
		maxOpenConns = 20
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	idleTime := pool.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = 5 * time.Minute
	}
	sqlDB.SetConnMaxIdleTime(idleTime)
	lifetime := pool.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = 30 * time.Minute
	}
	sqlDB.SetConnMaxLifetime(lifetime)
}

// newPostgresLogStore creates a new Postgres log store.
// Schema management is the caller's responsibility; Bifrost only opens a runtime pool.
func newPostgresLogStore(ctx context.Context, config *PostgresConfig, logger schemas.Logger) (LogStore, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	// Validate required config
	if config.Host == nil || config.Host.GetValue() == "" {
		return nil, fmt.Errorf("postgres host is required")
	}
	if config.Port == nil || config.Port.GetValue() == "" {
		return nil, fmt.Errorf("postgres port is required")
	}
	if config.User == nil || config.User.GetValue() == "" {
		return nil, fmt.Errorf("postgres user is required")
	}
	if config.Password == nil || config.Password.GetValue() == "" {
		return nil, fmt.Errorf("postgres password is required")
	}
	if config.DBName == nil || config.DBName.GetValue() == "" {
		return nil, fmt.Errorf("postgres db name is required")
	}
	if config.SSLMode == nil || config.SSLMode.GetValue() == "" {
		return nil, fmt.Errorf("postgres ssl mode is required")
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s", config.Host.GetValue(), config.Port.GetValue(), config.User.GetValue(), config.Password.GetValue(), config.DBName.GetValue(), config.SSLMode.GetValue())

	closePool := func(db *gorm.DB) error {
		if db == nil {
			return nil
		}
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}

	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger: newGormLogger(logger),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		closePool(db)
		return nil, err
	}

	applyLogStorePool(sqlDB, PoolSettings{
		MaxIdleConns:    config.MaxIdleConns,
		MaxOpenConns:    config.MaxOpenConns,
		ConnMaxIdleTime: config.ConnMaxIdleTime,
		ConnMaxLifetime: config.ConnMaxLifetime,
	})

	return &RDBLogStore{db: db, logger: logger}, nil
}

// NewPostgresLogStoreFromDSN opens a postgres log store from a libpq DSN.
// Schema management is the caller's responsibility; Bifrost only opens a runtime pool.
func NewPostgresLogStoreFromDSN(ctx context.Context, dsn string, pool PoolSettings, logger schemas.Logger) (LogStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres dsn is required")
	}
	_ = ctx

	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger: newGormLogger(logger),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		_ = sqlDBClose(db)
		return nil, err
	}

	applyLogStorePool(sqlDB, pool)

	return &RDBLogStore{db: db, logger: logger}, nil
}

func sqlDBClose(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
