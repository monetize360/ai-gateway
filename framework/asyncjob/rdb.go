package asyncjob

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gorm.io/gorm"
)

type rdbStore struct {
	db *gorm.DB
}

func applyPool(sqlDB *sql.DB, pool PoolSettings) {
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

func (s *rdbStore) Create(ctx context.Context, job *Job) error {
	return s.db.WithContext(ctx).Create(job).Error
}

func (s *rdbStore) FindByID(ctx context.Context, id string) (*Job, error) {
	var job Job
	result := s.db.WithContext(ctx).Where("id = ? AND (expires_at IS NULL OR expires_at > ?)", id, time.Now().UTC()).First(&job)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, result.Error
	}
	return &job, nil
}

func (s *rdbStore) Update(ctx context.Context, id string, updates map[string]any) error {
	return s.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).Updates(updates).Error
}

func (s *rdbStore) DeleteExpired(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	const batchLimit = 100
	var totalDeleted int64
	for {
		result := s.db.WithContext(ctx).
			Where("id IN (?)",
				s.db.Model(&Job{}).Select("id").
					Where("expires_at IS NOT NULL AND expires_at < ?", now).
					Limit(batchLimit),
			).Delete(&Job{})
		if result.Error != nil {
			return totalDeleted, result.Error
		}
		totalDeleted += result.RowsAffected
		if result.RowsAffected < batchLimit {
			break
		}
	}
	return totalDeleted, nil
}

func (s *rdbStore) DeleteStale(ctx context.Context, staleSince time.Time) (int64, error) {
	result := s.db.WithContext(ctx).
		Where("status = ? AND created_at < ?", "processing", staleSince).
		Delete(&Job{})
	return result.RowsAffected, result.Error
}

func (s *rdbStore) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (s *rdbStore) Close(ctx context.Context) error {
	_ = ctx
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
