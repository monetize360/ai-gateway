package guardrails

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/maximhq/bifrost/framework/configstore"
)

// nemoManagedMarker holds the owning tenant id. Folders without it are shipped configs
// and are never touched by the sync.
const nemoManagedMarker = ".m360-managed"

// syncNemoConfigs writes the tenant's enabled NeMo configs into the shared config
// directory. Each tenant gets its own folder prefix, so two tenants may reuse a config name.
func (p *Plugin) syncNemoConfigs(ctx context.Context, tenantID string, store configstore.ConfigStore) error {
	dir := strings.TrimSpace(p.config.NemoConfigDir)
	if dir == "" || store == nil || strings.TrimSpace(tenantID) == "" {
		return nil
	}
	rows, err := store.GetGuardrailNemoConfigs(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	kept := map[string]bool{}
	slugs := map[string]bool{}
	for _, row := range rows {
		if row.Deleted || (row.Enabled != nil && !*row.Enabled) {
			continue
		}
		slug := configSlug(row.Name)
		if slug == "" {
			continue
		}
		folderName := tenantNemoFolder(tenantID, slug)
		folder := filepath.Join(dir, folderName)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return err
		}
		if err := writeIfChanged(filepath.Join(folder, "config.yml"), row.ConfigYML); err != nil {
			return err
		}
		if err := writeIfChanged(filepath.Join(folder, "rails.co"), row.RailsCo); err != nil {
			return err
		}
		if err := writeIfChanged(filepath.Join(folder, nemoManagedMarker), tenantID); err != nil {
			return err
		}
		kept[folderName] = true
		slugs[slug] = true
	}
	p.nemoFolders.Store(tenantID, slugs)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || kept[entry.Name()] {
			continue
		}
		owner, readErr := os.ReadFile(filepath.Join(dir, entry.Name(), nemoManagedMarker))
		if errors.Is(readErr, os.ErrNotExist) || strings.TrimSpace(string(owner)) != tenantID {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// nemoFolder maps a policy's config_id to the folder NeMo loads. A tenant config resolves
// to that tenant's folder. Anything else is a shipped config and keeps its own name.
func (p *Plugin) nemoFolder(tenantID, configID string) string {
	slug := configSlug(configID)
	if value, ok := p.nemoFolders.Load(tenantID); ok {
		if slugs, ok := value.(map[string]bool); ok && slugs[slug] {
			return tenantNemoFolder(tenantID, slug)
		}
	}
	return strings.TrimSpace(configID)
}

func tenantNemoFolder(tenantID, slug string) string {
	return "t-" + configSlug(tenantID) + "--" + slug
}

// writeIfChanged leaves an unchanged file alone, so the NeMo watcher only restarts the
// server when a config really changed.
func writeIfChanged(path, contents string) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == contents {
		return nil
	}
	return os.WriteFile(path, []byte(contents), 0o644)
}

func configSlug(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
