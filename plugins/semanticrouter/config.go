package semanticrouter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultEntrypoint = "vllm-sr/auto"
)

// Config is the Bifrost plugin config for semantic-router.
type Config struct {
	RecipeFile string `json:"recipe_file"`
	Entrypoint string `json:"entrypoint,omitempty"`
}

func (c *Config) entrypoint() string {
	if c != nil && strings.TrimSpace(c.Entrypoint) != "" {
		return strings.TrimSpace(c.Entrypoint)
	}
	return defaultEntrypoint
}

func (c *Config) validate() error {
	if c == nil {
		return fmt.Errorf("semantic-router config is required")
	}
	if strings.TrimSpace(c.RecipeFile) == "" {
		return fmt.Errorf("semantic-router.recipe_file is required")
	}
	return nil
}

func resolveRecipePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("recipe_file is empty")
	}
	candidates := portablePathCandidates(path)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			return "", fmt.Errorf("%q is a directory", candidate)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("recipe file not found at %q (use a path relative to the process working directory / -app-dir)", path)
}

// portablePathCandidates returns the path plus relatives from cwd and ancestor dirs
// (air often runs with cwd = transports/bifrost-http while config paths are repo-root relative).
func portablePathCandidates(path string) []string {
	candidates := []string{path}
	if filepath.IsAbs(path) {
		return candidates
	}
	if abs, err := filepath.Abs(path); err == nil {
		candidates = append(candidates, abs)
	}
	wd, err := os.Getwd()
	if err != nil {
		return candidates
	}
	dir := wd
	for {
		candidates = append(candidates, filepath.Join(dir, path))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return candidates
}
