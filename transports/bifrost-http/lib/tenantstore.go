package lib

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

const (
	modelCatalogTenantPricingStartupSyncLock = "model_catalog_tenant_pricing_startup_sync"
	modelCatalogTenantParamsStartupSyncLock  = "model_catalog_tenant_params_startup_sync"
	distributedLockRetryAttempts             = 10
)

// TenantStoreFileConfig is the config.json shape for multi-tenant mode.
type TenantStoreFileConfig struct {
	Enabled                bool                           `json:"enabled"`
	RefreshIntervalSeconds int                            `json:"refresh_interval_seconds,omitempty"`
	Global                 *TenantStoreGlobalPostgresFile `json:"global,omitempty"`
	JWTPublicKey           string                         `json:"jwt_public_key,omitempty"`
	JWTSecret              string                         `json:"jwt_secret,omitempty"`
}

// TenantStoreGlobalPostgresFile holds plain-string postgres settings for the global DB.
type TenantStoreGlobalPostgresFile struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"db_name"`
	SSLMode  string `json:"ssl_mode"`
}

// TenantStoreHolder wires the per-tenant ConfigStore registry and JWT verification
// key into the HTTP transport layer.
type TenantStoreHolder struct {
	Registry           *tenantstore.TenantConfigRegistry
	Manager            *tenantstore.TenantDBManager
	GlobalDB           *tenantstore.GlobalDB
	JWTKey             []byte
	catalogLockManager *configstore.DistributedLockManager
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

// InitTenantStore bootstraps multi-tenant infrastructure when tenant_store.enabled
// is true in config.json.
func InitTenantStore(
	ctx context.Context,
	cfg *TenantStoreFileConfig,
	defaultStore configstore.ConfigStore,
	logger schemas.Logger,
) (*TenantStoreHolder, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
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

	manager := tenantstore.NewTenantDBManager(globalDB, logger)

	// Eagerly open a ConfigStore for every tenant in the global DB at startup.
	if err := manager.LoadAll(ctx); err != nil {
		return nil, fmt.Errorf("failed to load tenant stores: %w", err)
	}

	registry := tenantstore.NewTenantConfigRegistry(manager, defaultStore)

	holder := &TenantStoreHolder{
		Registry:           registry,
		Manager:            manager,
		GlobalDB:           globalDB,
		JWTKey:             jwtKey,
		catalogLockManager: newCatalogDistributedLockManager(defaultStore, logger),
	}

	// Best-effort: copy model pricing/parameters already present in the default
	// config store into each tenant DB. A follow-up sync runs after ModelCatalog
	// finishes any background cloud fetch (see WireTenantModelCatalogSync).
	holder.syncModelCatalogToTenantsLocked(ctx, defaultStore, logger)

	logger.Info("multi-tenant mode enabled")
	return holder, nil
}

func newCatalogDistributedLockManager(sourceStore configstore.ConfigStore, logger schemas.Logger) *configstore.DistributedLockManager {
	if sourceStore == nil {
		return nil
	}
	return configstore.NewDistributedLockManager(
		sourceStore,
		logger,
		configstore.WithDefaultTTL(30*time.Second),
	)
}

func withDistributedLock(
	ctx context.Context,
	lockManager *configstore.DistributedLockManager,
	logger schemas.Logger,
	key string,
	retries int,
	fn func() error,
) error {
	if lockManager == nil {
		return fn()
	}
	lock, err := lockManager.NewLock(key)
	if err != nil {
		return fmt.Errorf("failed to create lock %q: %w", key, err)
	}
	if retries > 0 {
		if err := lock.LockWithRetry(ctx, retries); err != nil {
			return fmt.Errorf("failed to acquire lock %q: %w", key, err)
		}
	} else if err := lock.Lock(ctx); err != nil {
		return fmt.Errorf("failed to acquire lock %q: %w", key, err)
	}
	defer func() {
		if err := lock.Unlock(context.Background()); err != nil {
			logger.Warn("failed to release distributed lock %q: %v", key, err)
		}
	}()
	return fn()
}

func (h *TenantStoreHolder) syncModelCatalogToTenantsLocked(
	ctx context.Context,
	sourceStore configstore.ConfigStore,
	logger schemas.Logger,
) {
	if h == nil || h.Manager == nil || sourceStore == nil {
		return
	}
	lockManager := h.catalogLockManager
	if lockManager == nil {
		lockManager = newCatalogDistributedLockManager(sourceStore, logger)
	}

	if err := withDistributedLock(ctx, lockManager, logger, modelCatalogTenantPricingStartupSyncLock, distributedLockRetryAttempts, func() error {
		tenantstore.SyncModelCatalogPricingToAllTenants(ctx, h.Manager, sourceStore, logger)
		return nil
	}); err != nil {
		logger.Warn("tenant model catalog pricing startup sync failed: %v", err)
	} else {
		logger.Info("tenant model catalog pricing startup sync completed successfully")
	}

	if err := withDistributedLock(ctx, lockManager, logger, modelCatalogTenantParamsStartupSyncLock, distributedLockRetryAttempts, func() error {
		tenantstore.SyncModelCatalogParametersToAllTenants(ctx, h.Manager, sourceStore, logger)
		return nil
	}); err != nil {
		logger.Warn("tenant model catalog parameters startup sync failed: %v", err)
	} else {
		logger.Info("tenant model catalog parameters startup sync completed successfully")
	}
}

// SyncModelCatalogToTenants copies model pricing and parameters from the default
// config store into every tenant database.
func (h *TenantStoreHolder) SyncModelCatalogToTenants(ctx context.Context, sourceStore configstore.ConfigStore, logger schemas.Logger) {
	h.syncModelCatalogToTenantsLocked(ctx, sourceStore, logger)
}

// WireTenantModelCatalogSync registers a ModelCatalog after-sync hook that
// replicates cloud-synced pricing/parameters into tenant databases, and waits
// for any startup background cloud fetch before running the first replication.
func WireTenantModelCatalogSync(
	ctx context.Context,
	holder *TenantStoreHolder,
	catalog *modelcatalog.ModelCatalog,
	sourceStore configstore.ConfigStore,
	logger schemas.Logger,
) {
	if holder == nil || catalog == nil || sourceStore == nil {
		return
	}

	syncTenants := func(syncCtx context.Context) {
		holder.syncModelCatalogToTenantsLocked(syncCtx, sourceStore, logger)
	}

	catalog.SetAfterSyncHook(syncTenants)

	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		catalog.WaitStartupBackgroundSync(waitCtx)
		syncTenants(waitCtx)
	}()
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
		Host:     schemas.NewEnvVar(cfg.Host),
		Port:     schemas.NewEnvVar(port),
		User:     schemas.NewEnvVar(cfg.User),
		Password: schemas.NewEnvVar(cfg.Password),
		DBName:   schemas.NewEnvVar(cfg.DBName),
		SSLMode:  schemas.NewEnvVar(sslMode),
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
