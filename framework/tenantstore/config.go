// Package tenantstore provides Database-per-Tenant connection management.
//
// It lazily initialises one configstore.ConfigStore per tenant on first access
// (double-checked locking), validates tenant JWTs (RS256 public key or HS256
// secret from config), and looks up per-tenant DB credentials from the mpilotv2
// global tenants table.
package tenantstore

import "github.com/maximhq/bifrost/core/schemas"

// Config holds the settings needed to bootstrap the tenantstore.
// The JWT secret is shared across all tenants; each tenant is identified
// by a "tenant_id" claim inside the JWT.
type Config struct {
	// JWTSecret is the HMAC-SHA256 key used to validate tenant access tokens.
	// Must be provided in the gateway config.json and is the same for every tenant.
	JWTSecret string `json:"jwt_secret"`

	// GlobalDB is the connection config for the control-plane database that
	// holds the tenants table (id → DSN mapping).
	GlobalDB *PostgresConfig `json:"global_db"`
}

// PostgresConfig mirrors configstore.PostgresConfig so the tenantstore package
// does not import configstore (avoiding a circular dependency).
type PostgresConfig struct {
	Host         *schemas.EnvVar `json:"host"`
	Port         *schemas.EnvVar `json:"port"`
	User         *schemas.EnvVar `json:"user"`
	Password     *schemas.EnvVar `json:"password"`
	DBName       *schemas.EnvVar `json:"db_name"`
	SSLMode      *schemas.EnvVar `json:"ssl_mode"`
	MaxIdleConns int             `json:"max_idle_conns"`
	MaxOpenConns int             `json:"max_open_conns"`
}

// DSN builds a libpq-style connection string from the config.
func (c *PostgresConfig) DSN() string {
	return "host=" + c.Host.GetValue() +
		" port=" + c.Port.GetValue() +
		" user=" + c.User.GetValue() +
		" password=" + c.Password.GetValue() +
		" dbname=" + c.DBName.GetValue() +
		" sslmode=" + c.SSLMode.GetValue()
}
