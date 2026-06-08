package lib

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

// TenantStoreFileConfig is the config.json shape for multi-tenant mode.
type TenantStoreFileConfig struct {
	Enabled                bool                           `json:"enabled"`
	RefreshIntervalSeconds int                            `json:"refresh_interval_seconds,omitempty"`
	MaxIdleConns           int                            `json:"max_idle_conns,omitempty"`
	MaxOpenConns           int                            `json:"max_open_conns,omitempty"`
	Global                 *TenantStoreGlobalPostgresFile `json:"global,omitempty"`
	JWTPublicKey           string                         `json:"jwt_public_key,omitempty"`
	JWTSecret              string                         `json:"jwt_secret,omitempty"`
}

// TenantStoreGlobalPostgresFile holds plain-string postgres settings for the global DB.
type TenantStoreGlobalPostgresFile struct {
	Host         string `json:"host"`
	Port         string `json:"port"`
	User         string `json:"user"`
	Password     string `json:"password"`
	DBName       string `json:"db_name"`
	SSLMode      string `json:"ssl_mode"`
	MaxIdleConns int    `json:"max_idle_conns,omitempty"`
	MaxOpenConns int    `json:"max_open_conns,omitempty"`
}

// TenantStoreHolder wires the per-tenant ConfigStore registry and JWT verification
// key into the HTTP transport layer.
type TenantStoreHolder struct {
	Registry tenantstore.Resolver
	Manager  *tenantstore.TenantDBManager
	GlobalDB *tenantstore.GlobalDB
	JWTKey   []byte
}

// Close releases global DB and per-tenant connection pools.
func (h *TenantStoreHolder) Close(ctx context.Context) {
	if h == nil {
		return
	}
	if h.Manager != nil {
		h.Manager.Close(ctx)
	}
	if h.GlobalDB != nil {
		_ = h.GlobalDB.Close()
	}
}

// InitTenantStore bootstraps multi-tenant infrastructure. tenant_store.enabled must
// be true; there is no default filesystem config store.
func InitTenantStore(
	ctx context.Context,
	cfg *TenantStoreFileConfig,
	logger schemas.Logger,
) (*TenantStoreHolder, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("tenant_store.enabled must be true")
	}
	if cfg.Global == nil {
		return nil, fmt.Errorf("tenant_store.global is required when tenant_store.enabled is true")
	}

	jwtKeyMaterial := cfg.JWTPublicKey
	if jwtKeyMaterial == "" {
		jwtKeyMaterial = cfg.JWTSecret
	}
	jwtKey := tenantstore.FormatJWTVerificationKey(jwtKeyMaterial)
	if len(jwtKey) == 0 {
		return nil, fmt.Errorf("tenant_store.jwt_public_key or tenant_store.jwt_secret is required")
	}

	globalCfg := postgresConfigFromFile(cfg.Global)
	globalDB, err := tenantstore.NewGlobalDB(ctx, globalCfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise global tenant DB: %w", err)
	}

	manager := tenantstore.NewTenantDBManager(globalDB, tenantPoolSettingsFromFile(cfg), logger)

	if err := manager.LoadAll(ctx); err != nil {
		return nil, fmt.Errorf("failed to load tenant stores: %w", err)
	}

	registry := tenantstore.NewTenantConfigRegistry(manager)

	holder := &TenantStoreHolder{
		Registry: registry,
		Manager:  manager,
		GlobalDB: globalDB,
		JWTKey:   jwtKey,
	}

	logger.Info("multi-tenant mode enabled")
	return holder, nil
}

func postgresConfigFromFile(cfg *TenantStoreGlobalPostgresFile) *tenantstore.PostgresConfig {
	if cfg == nil {
		return nil
	}
	sslMode := cfg.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	port := cfg.Port
	if port == "" {
		port = "5432"
	}
	return &tenantstore.PostgresConfig{
		Host:         schemas.NewEnvVar(cfg.Host),
		Port:         schemas.NewEnvVar(port),
		User:         schemas.NewEnvVar(cfg.User),
		Password:     schemas.NewEnvVar(cfg.Password),
		DBName:       schemas.NewEnvVar(cfg.DBName),
		SSLMode:      schemas.NewEnvVar(sslMode),
		MaxIdleConns: cfg.MaxIdleConns,
		MaxOpenConns: cfg.MaxOpenConns,
	}
}

func tenantPoolSettingsFromFile(cfg *TenantStoreFileConfig) configstore.PostgresPoolSettings {
	if cfg == nil {
		return configstore.PostgresPoolSettings{}
	}
	return configstore.PostgresPoolSettings{
		MaxIdleConns: cfg.MaxIdleConns,
		MaxOpenConns: cfg.MaxOpenConns,
	}
}

// GetTenantIDFromContext is a convenience helper for handlers and plugins
// that need to read the tenant ID without importing core/schemas directly.
func GetTenantIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	return v
}

// GetAccessKeyFromContext returns the JWT accessKey claim from context.
func GetAccessKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(schemas.BifrostContextKeyAccessKey).(string)
	return v
}

// TenantGovernanceSyncInterval returns how often per-tenant governance stores
// should be refreshed from the database. Defaults to 10s when unset.
func TenantGovernanceSyncInterval(cfg *TenantStoreFileConfig) time.Duration {
	const defaultInterval = 10 * time.Second
	if cfg == nil || cfg.RefreshIntervalSeconds <= 0 {
		return defaultInterval
	}
	return time.Duration(cfg.RefreshIntervalSeconds) * time.Second
}

// TenantPathSkipsJWT returns true for routes that do not require tenant JWT.
func TenantPathSkipsJWT(path string) bool {
	path = strings.TrimSuffix(path, "/")
	if path == "" || path == "/" {
		return true
	}
	if path == "/health" || path == "/metrics" || path == "/favicon.ico" || path == "/login" {
		return true
	}
	if strings.HasPrefix(path, "/assets/") {
		return true
	}
	switch path {
	case "/api/session/is-auth-enabled",
		"/api/session/login",
		"/api/oauth/callback",
		"/api/version":
		return true
	}
	if strings.HasPrefix(path, "/api/scim/oauth/") {
		return true
	}
	if strings.HasPrefix(path, "/api/dev") {
		return true
	}
	return false
}
