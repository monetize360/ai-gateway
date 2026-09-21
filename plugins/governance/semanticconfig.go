package governance

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// DeprecatedClassifierConfig is ignored at runtime. Retained so existing configs
// that still ship a classifier block continue to unmarshal.
type DeprecatedClassifierConfig struct {
	Provider  schemas.ModelProvider `json:"provider,omitempty"`
	Model     string                `json:"model,omitempty"`
	BaseURL   string                `json:"base_url,omitempty"`
	APIKey    string                `json:"api_key,omitempty"`
	TimeoutMs int                   `json:"timeout_ms,omitempty"`
}

func (c *DeprecatedClassifierConfig) enabled() bool {
	return c != nil && strings.TrimSpace(c.BaseURL) != ""
}

const (
	// Local routing work budget (capability filter + ranking). Router HTTP
	// time is separate and governed by router.timeout_ms.
	documentedLocalRoutingBudgetMs = 10

	// Warn when operators set a preview timeout far above a typical production SLO.
	routerTimeoutWarnMs = 5000
)

// validateAndNormalizeSemanticRouting checks router URL shape, resolves portable
// overrides paths, and emits operator warnings. Hard-fails only on invalid URL
// schemes or a missing overrides file.
func (c *SemanticRoutingConfig) validateAndNormalizeSemanticRouting(logger schemas.Logger) error {
	if c == nil {
		return nil
	}
	if err := c.validateRouter(logger); err != nil {
		return err
	}
	if err := c.resolveModelOverridesFilePath(logger); err != nil {
		return err
	}
	if c.Classifier != nil && c.Classifier.enabled() {
		if logger != nil {
			logger.Warn("governance semantic_routing.classifier is deprecated and ignored; Layer 2 uses router (vLLM-SR) only")
		}
	}
	return nil
}

func (c *SemanticRoutingConfig) validateRouter(logger schemas.Logger) error {
	if c == nil || c.Router == nil || !c.Router.enabled() {
		return nil
	}
	raw := strings.TrimSpace(c.Router.BaseURL)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("semantic_routing.router.base_url must be an absolute http(s) URL, got %q", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("semantic_routing.router.base_url must use http or https, got %q", parsed.Scheme)
	}

	if c.Router.TimeoutMs > routerTimeoutWarnMs && logger != nil {
		logger.Warn("governance semantic_routing.router.timeout_ms=%d exceeds %dms; local work budget is ~%dms and total semantic budget ≈ %dms + timeout_ms",
			c.Router.TimeoutMs, routerTimeoutWarnMs, documentedLocalRoutingBudgetMs, documentedLocalRoutingBudgetMs)
	}

	if logger != nil && isLoopbackHost(parsed.Hostname()) && !isLocalDevMarker() {
		logger.Warn("governance semantic_routing.router.base_url points at loopback (%s); use only for local validation (set BIFROST_ENV=local|dev to silence)", raw)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLocalDevMarker() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("BIFROST_ENV")))
	switch env {
	case "local", "dev", "development", "test":
		return true
	}
	return false
}

// resolveModelOverridesFilePath makes relative model_overrides_file paths portable
// by resolving against the process working directory (typically -app-dir).
func (c *SemanticRoutingConfig) resolveModelOverridesFilePath(logger schemas.Logger) error {
	if c == nil {
		return nil
	}
	path := strings.TrimSpace(c.ModelOverridesFile)
	if path == "" {
		return nil
	}
	resolved, err := resolvePortableFilePath(path)
	if err != nil {
		return fmt.Errorf("semantic_routing.model_overrides_file: %w", err)
	}
	if resolved != path && logger != nil {
		logger.Debug("governance resolved model_overrides_file %q → %q", path, resolved)
	}
	c.ModelOverridesFile = resolved
	return nil
}

func resolvePortableFilePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			candidates = append(candidates, abs)
		}
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(wd, path))
		}
	}
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
	return "", fmt.Errorf("file not found at %q (use a path relative to the process working directory / -app-dir)", path)
}
