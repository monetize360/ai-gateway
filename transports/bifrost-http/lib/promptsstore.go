package lib

import (
	"context"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// TenantPromptsStore resolves prompts from the per-tenant config store using
// tenant ID in ctx. When no tenant is present (e.g. at boot), queries return
// empty results so the prompts plugin can start with an empty cache.
type TenantPromptsStore struct {
	cfg *Config
}

// NewTenantPromptsStore returns a store adapter for the prompts plugin.
func NewTenantPromptsStore(cfg *Config) *TenantPromptsStore {
	return &TenantPromptsStore{cfg: cfg}
}

// GetPrompts returns all prompts for the tenant in ctx.
func (s *TenantPromptsStore) GetPrompts(ctx context.Context, folderID *string) ([]configstoreTables.TablePrompt, error) {
	store := s.cfg.StoreFromContext(ctx)
	if store == nil {
		return []configstoreTables.TablePrompt{}, nil
	}
	return store.GetPrompts(ctx, folderID)
}

// GetAllPromptVersions returns all prompt versions for the tenant in ctx.
func (s *TenantPromptsStore) GetAllPromptVersions(ctx context.Context) ([]configstoreTables.TablePromptVersion, error) {
	store := s.cfg.StoreFromContext(ctx)
	if store == nil {
		return []configstoreTables.TablePromptVersion{}, nil
	}
	return store.GetAllPromptVersions(ctx)
}
