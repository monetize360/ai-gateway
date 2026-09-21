package asyncjob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/valyala/fasthttp"
)

const (
	DefaultAsyncJobResultTTL = 3600

	asyncJobCleanupInterval      = 1 * time.Minute
	asyncJobCleanupTimeout       = 1 * time.Minute
	asyncJobStaleProcessingHours = 24
)

// AsyncOperation runs in the background and returns a response or BifrostError.
type AsyncOperation func(ctx *schemas.BifrostContext) (any, *schemas.BifrostError)

// GovernanceStore looks up virtual keys for job ownership checks.
type GovernanceStore interface {
	GetVirtualKey(ctx context.Context, vkValue string) (*configstoreTables.TableVirtualKey, bool)
}

// Executor manages async job creation and background execution.
type Executor struct {
	store           Store
	resolver        Resolver
	governanceStore GovernanceStore
	logger          schemas.Logger
}

// NewExecutor creates an executor. Provide a single store and/or a tenant resolver.
func NewExecutor(store Store, resolver Resolver, governanceStore GovernanceStore, logger schemas.Logger) *Executor {
	return &Executor{
		store:           store,
		resolver:        resolver,
		governanceStore: governanceStore,
		logger:          logger,
	}
}

func (e *Executor) storeFor(ctx context.Context) Store {
	if e.resolver != nil {
		if store := e.resolver.GetStoreFromContext(ctx); store != nil {
			return store
		}
	}
	return e.store
}

// RetrieveJob retrieves a job by its ID.
func (e *Executor) RetrieveJob(ctx context.Context, jobID string, vkValue *string, operationType schemas.RequestType) (*Job, error) {
	store := e.storeFor(ctx)
	if store == nil {
		return nil, fmt.Errorf("async job store is not configured")
	}
	job, err := store.FindByID(ctx, jobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("job not found or expired")
		}
		return nil, fmt.Errorf("%w: %w", ErrJobInternal, err)
	}
	if job.VirtualKeyID != nil {
		if vkValue == nil {
			return nil, fmt.Errorf("virtual key is required")
		}
		vk, ok := e.governanceStore.GetVirtualKey(ctx, *vkValue)
		if !ok {
			return nil, fmt.Errorf("virtual key not found")
		}
		if *job.VirtualKeyID != vk.ID {
			return nil, fmt.Errorf("virtual key mismatch")
		}
	}
	if job.RequestType != operationType {
		return nil, fmt.Errorf("operation type mismatch")
	}
	return job, nil
}

// SubmitJob creates a pending job, starts background execution, and returns the job record.
func (e *Executor) SubmitJob(bifrostCtx *schemas.BifrostContext, resultTTL int, operation AsyncOperation, operationType schemas.RequestType) (*Job, error) {
	if resultTTL <= 0 {
		resultTTL = DefaultAsyncJobResultTTL
	}

	store := e.storeFor(bifrostCtx)
	if store == nil {
		return nil, fmt.Errorf("async job store is not configured")
	}

	virtualKeyValue := getVirtualKeyFromContext(bifrostCtx)

	var virtualKeyID *string
	if virtualKeyValue != nil {
		vk, ok := e.governanceStore.GetVirtualKey(bifrostCtx, *virtualKeyValue)
		if !ok {
			return nil, fmt.Errorf("virtual key not found")
		}
		virtualKeyID = &vk.ID
	}

	now := time.Now().UTC()
	job := &Job{
		ID:           uuid.NewString(),
		Status:       schemas.AsyncJobStatusPending,
		RequestType:  operationType,
		VirtualKeyID: virtualKeyID,
		ResultTTL:    resultTTL,
		CreatedAt:    now,
	}

	if err := store.Create(context.Background(), job); err != nil {
		return nil, fmt.Errorf("failed to create async job: %w", err)
	}

	var contextValues map[any]any
	if bifrostCtx != nil {
		contextValues = bifrostCtx.GetUserValues()
	}
	go e.executeJob(store, job.ID, job.ResultTTL, operation, contextValues)

	return job, nil
}

func (e *Executor) executeJob(store Store, jobID string, resultTTL int, operation AsyncOperation, contextValues map[any]any) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	for k, v := range contextValues {
		ctx.SetValue(k, v)
	}

	ctx.ClearValue(schemas.BifrostContextKeyTraceID)
	ctx.ClearValue(schemas.BifrostContextKeyParentSpanID)
	ctx.ClearValue(schemas.BifrostContextKeySpanID)

	updateJob := func(updates map[string]any) error {
		currentStore := e.storeFor(ctx)
		if currentStore == nil {
			currentStore = store
		}
		if currentStore == nil {
			return fmt.Errorf("async job store is not configured")
		}
		return currentStore.Update(ctx, jobID, updates)
	}

	markFailed := func(msg string) {
		now := time.Now().UTC()
		expiresAt := now.Add(time.Duration(resultTTL) * time.Second)
		errJSON, _ := sonic.Marshal(&schemas.BifrostError{Error: &schemas.ErrorField{Message: msg}})
		if err := updateJob(map[string]any{
			"status":       schemas.AsyncJobStatusFailed,
			"status_code":  fasthttp.StatusInternalServerError,
			"error":        string(errJSON),
			"completed_at": now,
			"expires_at":   expiresAt,
		}); err != nil {
			e.logger.Warn("failed to update async job to failed: %v", err)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			e.logger.Warn("async job %s panicked: %v", jobID, r)
			markFailed(fmt.Sprintf("internal error: %v", r))
		}
	}()

	if err := updateJob(map[string]any{
		"status": schemas.AsyncJobStatusProcessing,
	}); err != nil {
		e.logger.Warn("failed to update async job: %v", err)
	}

	ctx.SetValue(schemas.BifrostIsAsyncRequest, true)

	resp, bifrostErr := operation(ctx)

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(resultTTL) * time.Second)

	if bifrostErr != nil {
		errJSON, err := sonic.Marshal(bifrostErr)
		if err != nil {
			e.logger.Warn("failed to marshal bifrost error: %v", err)
			markFailed(fmt.Sprintf("failed to serialize error response: %v", err))
			return
		}
		statusCode := fasthttp.StatusInternalServerError
		if bifrostErr.StatusCode != nil {
			statusCode = *bifrostErr.StatusCode
		}
		if err := updateJob(map[string]any{
			"status":       schemas.AsyncJobStatusFailed,
			"status_code":  statusCode,
			"error":        string(errJSON),
			"completed_at": now,
			"expires_at":   expiresAt,
		}); err != nil {
			e.logger.Warn("failed to update async job: %v", err)
		}
		return
	}

	respJSON, err := sonic.Marshal(resp)
	if err != nil {
		e.logger.Warn("failed to marshal result: %v", err)
		markFailed(fmt.Sprintf("failed to serialize result: %v", err))
		return
	}
	if err := updateJob(map[string]any{
		"status":       schemas.AsyncJobStatusCompleted,
		"status_code":  fasthttp.StatusOK,
		"response":     string(respJSON),
		"completed_at": now,
		"expires_at":   expiresAt,
	}); err != nil {
		e.logger.Warn("failed to update async job: %v", err)
	}
}

// Cleaner periodically deletes expired and stale async jobs.
type Cleaner struct {
	store       Store
	resolver    Resolver
	logger      schemas.Logger
	stopCleanup chan struct{}
	mu          sync.Mutex
}

func NewCleaner(store Store, resolver Resolver, logger schemas.Logger) *Cleaner {
	return &Cleaner{
		store:    store,
		resolver: resolver,
		logger:   logger,
	}
}

func (c *Cleaner) StartCleanupRoutine() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopCleanup != nil {
		return
	}

	c.stopCleanup = make(chan struct{})
	stopCh := c.stopCleanup

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), asyncJobCleanupTimeout)
		c.cleanupExpiredJobs(ctx)
		cancel()

		ticker := time.NewTicker(asyncJobCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), asyncJobCleanupTimeout)
				c.cleanupExpiredJobs(ctx)
				cancel()
			case <-stopCh:
				c.logger.Debug("async job cleanup routine stopped")
				return
			}
		}
	}()
	c.logger.Debug("async job cleanup routine started (interval: %s)", asyncJobCleanupInterval)
}

func (c *Cleaner) StopCleanupRoutine() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopCleanup == nil {
		c.logger.Debug("async job cleanup routine already stopped")
		return
	}

	close(c.stopCleanup)
	c.stopCleanup = nil
}

func (c *Cleaner) cleanupExpiredJobs(ctx context.Context) {
	run := func(store Store) {
		if store == nil {
			return
		}
		deleted, err := store.DeleteExpired(ctx)
		if err != nil {
			c.logger.Warn("failed to delete expired async jobs: %v", err)
		} else if deleted > 0 {
			c.logger.Debug("async job cleanup completed: deleted %d expired jobs", deleted)
		}

		staleSince := time.Now().UTC().Add(-asyncJobStaleProcessingHours * time.Hour)
		staleDeleted, err := store.DeleteStale(ctx, staleSince)
		if err != nil {
			c.logger.Warn("failed to delete stale processing async jobs: %v", err)
		} else if staleDeleted > 0 {
			c.logger.Warn("async job cleanup: deleted %d stale processing jobs (stuck > %dh)", staleDeleted, asyncJobStaleProcessingHours)
		}
	}

	if c.resolver != nil {
		c.resolver.ForEachStore(func(_ string, store Store) {
			run(store)
		})
		return
	}
	run(c.store)
}

func getVirtualKeyFromContext(ctx *schemas.BifrostContext) *string {
	if ctx == nil {
		return nil
	}
	vkValue := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyVirtualKey)
	if vkValue == "" {
		return nil
	}
	return &vkValue
}
