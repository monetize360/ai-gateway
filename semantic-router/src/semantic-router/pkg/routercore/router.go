//go:build cgo

package routercore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/extproc"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

// Router is the reusable, non-serving Semantic Router API.
type Router interface {
	Preview(context.Context, services.IntentRequest) (*services.EvalResponse, error)
	Reload(context.Context, *config.RouterConfig) error
	Close() error
}

// Options configures an embedded router.
type Options struct {
	Entrypoint string
	Warmup     *services.IntentRequest
}

// Core owns one immutable routing-runtime generation. Reload constructs the
// replacement completely before publishing it so requests never see a partial
// classifier or selector graph.
type Core struct {
	mu         sync.RWMutex
	router     *extproc.OpenAIRouter
	entrypoint string
	closed     bool
}

// NewFromFile parses canonical Semantic Router configuration without using the
// package-global config singleton, then initializes all required native models.
func NewFromFile(ctx context.Context, path string, options Options) (*Core, error) {
	cfg, err := config.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse semantic router config: %w", err)
	}
	core, err := New(ctx, cfg, options)
	if err != nil {
		return nil, err
	}
	return core, nil
}

// New initializes a reusable routing core from canonical configuration.
func New(ctx context.Context, cfg *config.RouterConfig, options Options) (*Core, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	router, err := extproc.NewOpenAIRouterFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize semantic router runtime: %w", err)
	}
	core := &Core{router: router, entrypoint: options.Entrypoint}
	if options.Warmup != nil {
		if _, err := core.Preview(ctx, *options.Warmup); err != nil {
			_ = router.Close()
			return nil, fmt.Errorf("warm semantic router runtime: %w", err)
		}
	}
	return core, nil
}

// Preview evaluates the same signal/decision/selection pipeline used by the
// standalone routing-preview endpoint.
func (c *Core) Preview(ctx context.Context, request services.IntentRequest) (*services.EvalResponse, error) {
	if c == nil {
		return nil, errors.New("semantic router core is nil")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || c.router == nil || c.router.ClassificationService == nil {
		return nil, errors.New("semantic router core is closed")
	}
	if request.Model == "" {
		request.Model = c.entrypoint
	}
	return c.router.ClassificationService.ClassifyIntentForEval(ctx, request)
}

// Reload atomically replaces the complete routing runtime.
func (c *Core) Reload(ctx context.Context, cfg *config.RouterConfig) error {
	if c == nil {
		return errors.New("semantic router core is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	replacement, err := extproc.NewOpenAIRouterFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("initialize replacement semantic router runtime: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = replacement.Close()
		return errors.New("semantic router core is closed")
	}
	previous := c.router
	c.router = replacement
	c.mu.Unlock()

	if previous != nil {
		return previous.Close()
	}
	return nil
}

// Close releases native classifiers, embeddings, selector state, and caches.
func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	router := c.router
	c.router = nil
	c.mu.Unlock()
	if router == nil {
		return nil
	}
	return router.Close()
}

var _ Router = (*Core)(nil)
