package tenantstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// tenantRow reads the columns we need from the mpilotv2 `tenants` table.
// Java source: com.monetize360.mpillotv2.common.model.tenants.Tenant
type tenantRow struct {
	ID         string `gorm:"column:id"`
	DbURL      string `gorm:"column:db_url"`      // jdbc:postgresql://host:port/dbname — only the db name is used
	DbUsername string `gorm:"column:db_username"` // per-tenant DB user (may differ across tenants)
	DbPassword string `gorm:"column:db_password"` // per-tenant DB password
}

// GlobalDB wraps the control-plane (mpilotv2 global) Postgres connection.
// It reads the tenants table to discover the database name and credentials for each
// tenant. Host, port, and SSL mode come from config.json; username and password
// come from the tenants table (each tenant may have different credentials).
type GlobalDB struct {
	db        *gorm.DB
	globalCfg *PostgresConfig // host/port/ssl_mode from config.json
	logger    schemas.Logger
}

// NewGlobalDB opens a connection to the global DB and validates the config.
func NewGlobalDB(ctx context.Context, cfg *PostgresConfig, logger schemas.Logger) (*GlobalDB, error) {
	if cfg == nil {
		return nil, fmt.Errorf("global DB config is required")
	}
	if cfg.Host == nil || cfg.Host.GetValue() == "" {
		return nil, fmt.Errorf("global DB host is required")
	}
	if cfg.Port == nil || cfg.Port.GetValue() == "" {
		return nil, fmt.Errorf("global DB port is required")
	}
	if cfg.User == nil || cfg.User.GetValue() == "" {
		return nil, fmt.Errorf("global DB user is required")
	}
	if cfg.Password == nil {
		return nil, fmt.Errorf("global DB password is required")
	}
	if cfg.DBName == nil || cfg.DBName.GetValue() == "" {
		return nil, fmt.Errorf("global DB name is required")
	}
	if cfg.SSLMode == nil || cfg.SSLMode.GetValue() == "" {
		return nil, fmt.Errorf("global DB ssl_mode is required")
	}

	db, err := gorm.Open(postgres.New(postgres.Config{DSN: cfg.DSN()}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open global DB: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get sql.DB from global DB: %w", err)
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle == 0 {
		maxIdle = 5
	}
	maxOpen := cfg.MaxOpenConns
	if maxOpen == 0 {
		maxOpen = 20
	}
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetMaxOpenConns(maxOpen)
	idleTime := cfg.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = 5 * time.Minute
	}
	sqlDB.SetConnMaxIdleTime(idleTime)
	lifetime := cfg.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = 30 * time.Minute
	}
	sqlDB.SetConnMaxLifetime(lifetime)

	logger.Info("global DB connected (%s)", cfg.DBName.GetValue())
	return &GlobalDB{db: db, globalCfg: cfg, logger: logger}, nil
}

// GetAllTenants reads every non-deleted tenant from the tenants table and
// returns a map of tenantID → libpq DSN built from the global config credentials.
// Only the database name is taken from each tenant row (via db_url).
func (g *GlobalDB) GetAllTenants(ctx context.Context) (map[string]string, error) {
	var rows []tenantRow
	if err := g.db.WithContext(ctx).
		Table("tenants").
		Select("id::text AS id, db_url, db_username, db_password").
		Where("deleted = ?", false).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("failed to query tenants table: %w", err)
	}

	result := make(map[string]string, len(rows))
	for _, row := range rows {
		dbName, err := dbNameFromURL(row.DbURL)
		if err != nil {
			g.logger.Warn("skipping tenant %s: cannot extract db_name from %q: %v", row.ID, row.DbURL, err)
			continue
		}
		result[row.ID] = g.buildDSN(dbName, row.DbUsername, row.DbPassword)
	}
	return result, nil
}

// GetTenantDSN returns the libpq DSN for a single tenant (used by lazy fallback or admin).
func (g *GlobalDB) GetTenantDSN(ctx context.Context, tenantID string) (string, error) {
	var row tenantRow
	if err := g.db.WithContext(ctx).
		Table("tenants").
		Select("id::text AS id, db_url, db_username, db_password").
		Where("id::text = ? AND deleted = ?", tenantID, false).
		First(&row).Error; err != nil {
		return "", fmt.Errorf("tenant %q not found: %w", tenantID, err)
	}
	dbName, err := dbNameFromURL(row.DbURL)
	if err != nil {
		return "", fmt.Errorf("tenant %q has invalid db_url %q: %w", tenantID, row.DbURL, err)
	}
	return g.buildDSN(dbName, row.DbUsername, row.DbPassword), nil
}

// buildDSN constructs a libpq DSN using:
//   - host, port, ssl_mode from config.json (global config)
//   - dbName, user, password from the per-tenant tenants table row
func (g *GlobalDB) buildDSN(dbName, user, password string) string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		g.globalCfg.Host.GetValue(),
		g.globalCfg.Port.GetValue(),
		user,
		password,
		dbName,
		g.globalCfg.SSLMode.GetValue(),
	)
}

// dbNameFromURL extracts the database name from a JDBC or plain postgres URL.
// jdbc:postgresql://host:5432/mydb?foo=bar  →  "mydb"
// postgres://host/mydb                       →  "mydb"
func dbNameFromURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("db_url is empty")
	}
	// Strip jdbc: prefix so we can work with the path.
	s := strings.TrimPrefix(rawURL, "jdbc:")
	// Find the path component after the host section.
	// After stripping "jdbc:" we have "postgresql://host:port/dbname?..."
	slash := strings.Index(s, "//")
	if slash < 0 {
		return "", fmt.Errorf("missing // in db_url: %q", rawURL)
	}
	rest := s[slash+2:] // "host:port/dbname?..."
	pathStart := strings.Index(rest, "/")
	if pathStart < 0 {
		return "", fmt.Errorf("no database name in db_url: %q", rawURL)
	}
	dbAndQuery := rest[pathStart+1:] // "dbname?..."
	dbName, _, _ := strings.Cut(dbAndQuery, "?")
	dbName = strings.TrimSpace(dbName)
	if dbName == "" {
		return "", fmt.Errorf("empty database name in db_url: %q", rawURL)
	}
	return dbName, nil
}

// Close releases the underlying database connection.
func (g *GlobalDB) Close() error {
	sqlDB, err := g.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
