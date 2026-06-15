package lib

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
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
	// AdminJWTPublicKey is the MPilot admin API RSA public key (jwt.key.public).
	// When set, tenant admin routes accept Bearer tokens that carry only tenantId.
	AdminJWTPublicKey string `json:"admin_jwt_public_key,omitempty"`
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
	Registry        tenantstore.Resolver
	Manager         *tenantstore.TenantDBManager
	LogStoreManager tenantstore.LogStoreResolver
	GlobalDB        *tenantstore.GlobalDB
	JWTKey          []byte
	AdminJWTKey     []byte
}

// Close releases global DB and per-tenant connection pools.
func (h *TenantStoreHolder) Close(ctx context.Context) {
	if h == nil {
		return
	}
	if h.Manager != nil {
		h.Manager.Close(ctx)
	}
	if h.LogStoreManager != nil {
		h.LogStoreManager.Close(ctx)
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
	adminJWTKey := tenantstore.FormatJWTVerificationKey(cfg.AdminJWTPublicKey)

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
		Registry:    registry,
		Manager:     manager,
		GlobalDB:    globalDB,
		JWTKey:      jwtKey,
		AdminJWTKey: adminJWTKey,
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

// LogStorePostgresConfigFromTenantStore builds a logs_store Postgres config from
// tenant_store.global (and top-level tenant_store pool defaults when global omits them).
func LogStorePostgresConfigFromTenantStore(cfg *TenantStoreFileConfig) (*logstore.PostgresConfig, error) {
	if cfg == nil || cfg.Global == nil {
		return nil, fmt.Errorf("tenant_store.global is required")
	}
	base := postgresConfigFromFile(cfg.Global)
	maxIdle := cfg.Global.MaxIdleConns
	maxOpen := cfg.Global.MaxOpenConns
	if maxIdle == 0 {
		maxIdle = cfg.MaxIdleConns
	}
	if maxOpen == 0 {
		maxOpen = cfg.MaxOpenConns
	}
	return &logstore.PostgresConfig{
		Host:         base.Host,
		Port:         base.Port,
		User:         base.User,
		Password:     base.Password,
		DBName:       base.DBName,
		SSLMode:      base.SSLMode,
		MaxIdleConns: maxIdle,
		MaxOpenConns: maxOpen,
	}, nil
}

// MergeLogsStorePostgresFromTenantStore fills unset logs_store postgres fields from
// tenant_store.global. Explicit logs_store.config values take precedence.
func MergeLogsStorePostgresFromTenantStore(pg *logstore.PostgresConfig, ts *TenantStoreFileConfig) error {
	if pg == nil {
		return fmt.Errorf("logs_store postgres config is nil")
	}
	defaults, err := LogStorePostgresConfigFromTenantStore(ts)
	if err != nil {
		if logsStorePostgresConfigComplete(pg) {
			return nil
		}
		return fmt.Errorf("logs_store postgres config is incomplete and %w", err)
	}
	envOrDefault := func(current, fallback *schemas.EnvVar) *schemas.EnvVar {
		if current != nil && current.GetValue() != "" {
			return current
		}
		return fallback
	}
	pg.Host = envOrDefault(pg.Host, defaults.Host)
	pg.Port = envOrDefault(pg.Port, defaults.Port)
	pg.User = envOrDefault(pg.User, defaults.User)
	pg.Password = envOrDefault(pg.Password, defaults.Password)
	pg.DBName = envOrDefault(pg.DBName, defaults.DBName)
	pg.SSLMode = envOrDefault(pg.SSLMode, defaults.SSLMode)
	if pg.MaxIdleConns == 0 {
		pg.MaxIdleConns = defaults.MaxIdleConns
	}
	if pg.MaxOpenConns == 0 {
		pg.MaxOpenConns = defaults.MaxOpenConns
	}
	if !logsStorePostgresConfigComplete(pg) {
		return fmt.Errorf("logs_store postgres config is incomplete after applying tenant_store.global defaults")
	}
	return nil
}

func logsStorePostgresConfigComplete(pg *logstore.PostgresConfig) bool {
	if pg == nil {
		return false
	}
	return pg.Host != nil && pg.Host.GetValue() != "" &&
		pg.Port != nil && pg.Port.GetValue() != "" &&
		pg.User != nil && pg.User.GetValue() != "" &&
		pg.Password != nil && pg.Password.GetValue() != "" &&
		pg.DBName != nil && pg.DBName.GetValue() != "" &&
		pg.SSLMode != nil && pg.SSLMode.GetValue() != ""
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

// GetVirtualKeyFromContext returns the JWT virtualKey claim from context.
func GetVirtualKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, _ := ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID).(string); v != "" {
		return v
	}
	v, _ := ctx.Value(schemas.BifrostContextKeyVirtualKey).(string)
	return v
}

// InitTenantLogStores opens per-tenant postgres log stores when logs_store is enabled.
func InitTenantLogStores(ctx context.Context, holder *TenantStoreHolder, logsConfig *logstore.Config) error {
	if holder == nil || holder.GlobalDB == nil || logsConfig == nil || !logsConfig.Enabled {
		return nil
	}
	if logsConfig.Type != logstore.LogStoreTypePostgres {
		return nil
	}
	pool := tenantstore.LogStorePoolSettings{MaxIdleConns: 5, MaxOpenConns: 50}
	if pg, ok := logsConfig.Config.(*logstore.PostgresConfig); ok && pg != nil {
		if pg.MaxIdleConns > 0 {
			pool.MaxIdleConns = pg.MaxIdleConns
		}
		if pg.MaxOpenConns > 0 {
			pool.MaxOpenConns = pg.MaxOpenConns
		}
	}
	manager := tenantstore.NewTenantLogStoreManager(holder.GlobalDB, pool, logger)
	if err := manager.LoadAll(ctx); err != nil {
		return err
	}
	holder.LogStoreManager = manager
	return nil
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
