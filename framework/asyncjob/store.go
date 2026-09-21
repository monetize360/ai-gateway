package asyncjob

import (
	"context"
	"time"
)

// Store persists async inference jobs.
type Store interface {
	Create(ctx context.Context, job *Job) error
	FindByID(ctx context.Context, id string) (*Job, error)
	Update(ctx context.Context, id string, updates map[string]any) error
	DeleteExpired(ctx context.Context) (int64, error)
	DeleteStale(ctx context.Context, staleSince time.Time) (int64, error)
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}

// Resolver opens a tenant-scoped Store from request context.
type Resolver interface {
	GetStoreFromContext(ctx context.Context) Store
	ForEachStore(fn func(tenantID string, store Store))
	SyncTenantsFromGlobalDB(ctx context.Context) error
	EvictIdle(ctx context.Context, idleTimeout time.Duration) []string
	Close(ctx context.Context)
}

// PoolSettings tunes sql.DB limits for an async job connection pool.
type PoolSettings struct {
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}
