package logstore

import (
	"context"
	"database/sql"
	"fmt"

	"gorm.io/gorm"
)

const (
	// indexAdvisoryLockKey serializes the background index build across cluster nodes.
	indexAdvisoryLockKey = int64(1000002)

	// matviewRefreshAdvisoryLockKey serializes periodic materialized view refreshes.
	matviewRefreshAdvisoryLockKey = int64(1000005)

	// matviewEnsureAdvisoryLockKey serializes startup materialized view creation/repair.
	matviewEnsureAdvisoryLockKey = int64(1000006)
)

// advisoryLock holds a dedicated connection and an advisory lock key so the
// lock remains on the same Postgres session throughout its lifetime.
type advisoryLock struct {
	conn    *sql.Conn
	lockKey int64
}

// release unlocks and returns the connection to the pool.
func (l *advisoryLock) release(ctx context.Context) {
	if l.conn == nil {
		return
	}
	_, _ = l.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.lockKey)
	_ = l.conn.Close()
}

// acquireAdvisoryLock gets a dedicated connection and attempts a non-blocking
// advisory lock. Returns a no-op lock for non-Postgres databases.
func acquireAdvisoryLock(ctx context.Context, db *gorm.DB, lockKey int64, label string) (*advisoryLock, error) {
	if db.Dialector.Name() != "postgres" {
		return &advisoryLock{}, nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB for %s lock: %w", label, err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("get connection for %s lock: %w", label, err)
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&acquired); err != nil {
		conn.Close()
		return nil, fmt.Errorf("try advisory lock (%s): %w", label, err)
	}
	if !acquired {
		conn.Close()
		return nil, fmt.Errorf("%s advisory lock held by another node", label)
	}
	return &advisoryLock{conn: conn, lockKey: lockKey}, nil
}

// acquireIndexLock acquires the advisory lock used to serialize index builds.
// Returns a no-op lock for non-Postgres databases.
func acquireIndexLock(ctx context.Context, db *gorm.DB) (*advisoryLock, error) {
	return acquireAdvisoryLock(ctx, db, indexAdvisoryLockKey, "index-build")
}

// ensureMetadataGINIndex creates a GIN index on the logs.metadata column for
// efficient JSONB querying. Idempotent — uses CREATE INDEX IF NOT EXISTS.
func ensureMetadataGINIndex(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_metadata_gin
		ON logs USING GIN (metadata)
	`)
	if err != nil {
		return fmt.Errorf("create metadata GIN index: %w", err)
	}
	return nil
}

// ensureDashboardEnhancements creates additional indexes that improve dashboard
// query performance. Idempotent.
func ensureDashboardEnhancements(ctx context.Context, conn *sql.Conn) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_logs_timestamp_provider ON logs (timestamp, provider)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_timestamp_model    ON logs (timestamp, model)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("dashboard enhancement index: %w", err)
		}
	}
	return nil
}

// ensurePerformanceIndexes creates composite indexes used by hot read paths.
// Idempotent.
func ensurePerformanceIndexes(ctx context.Context, conn *sql.Conn) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_logs_provider_timestamp ON logs (provider, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model_timestamp    ON logs (model, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_status_timestamp   ON logs (status, timestamp DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("performance index: %w", err)
		}
	}
	return nil
}
