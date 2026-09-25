//go:build cgo

package semanticrouter

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/routercore"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

// Plugin owns the canonical in-process Semantic Router runtime.
type Plugin struct {
	cfg    Config
	logger schemas.Logger
	core   routercore.Router
}

// Init loads the canonical Semantic Router configuration and all native
// classifier/selector resources required by reachable recipes.
func Init(cfg *Config, logger schemas.Logger) (*Plugin, error) {
	if cfg == nil {
		return nil, fmt.Errorf("semantic-router config is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	path, err := resolveRecipePath(cfg.RecipeFile)
	if err != nil {
		return nil, err
	}
	core, err := routercore.NewFromFile(context.Background(), path, routercore.Options{
		Entrypoint: cfg.entrypoint(),
		Warmup: &services.IntentRequest{
			Model: cfg.entrypoint(),
			Text:  "semantic router startup warmup",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize embedded semantic router: %w", err)
	}
	if logger != nil {
		logger.Info("semantic-router initialized embedded runtime from %q", path)
	}
	return &Plugin{
		cfg:    *cfg,
		logger: logger,
		core:   core,
	}, nil
}

func (p *Plugin) GetName() string { return PluginName }

func (p *Plugin) Cleanup() error {
	if p == nil || p.core == nil {
		return nil
	}
	return p.core.Close()
}

// Preview implements Router.
func (p *Plugin) Preview(ctx context.Context, in PreviewInput) (*PreviewResult, error) {
	if p == nil {
		return nil, fmt.Errorf("semantic-router plugin is nil")
	}
	if p.core == nil {
		return nil, fmt.Errorf("semantic-router runtime not loaded")
	}
	request, err := toIntentRequest(in, p.cfg.entrypoint())
	if err != nil {
		return nil, err
	}
	response, err := p.core.Preview(ctx, request)
	if err != nil {
		return nil, err
	}
	return fromEvalResponse(response), nil
}
