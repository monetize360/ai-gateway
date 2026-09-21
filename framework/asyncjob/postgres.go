package asyncjob

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// NewPostgresStoreFromDSN opens a postgres async job store from a libpq DSN.
func NewPostgresStoreFromDSN(ctx context.Context, dsn string, pool PoolSettings, logger schemas.Logger) (Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres dsn is required")
	}
	_ = ctx
	_ = logger

	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		if closeErr := sqlDBClose(db); closeErr != nil && logger != nil {
			logger.Warn("failed to close async job db after ping setup error: %v", closeErr)
		}
		return nil, err
	}
	applyPool(sqlDB, pool)
	return &rdbStore{db: db}, nil
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
