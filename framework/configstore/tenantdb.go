// Package configstore — tenant-aware store helpers.
//
// Per-tenant ConfigStore instances are created by
// NewPostgresConfigStoreFromDSN (see postgres.go), which accepts a pre-built
// connection string and PostgresPoolSettings for sql.DB pool tuning. Tenant schema
// is owned by MPilot Liquibase; Bifrost does not run triggerMigrations on
// tenant databases. Used by framework/tenantstore.TenantDBManager at startup.
//
// Higher-level context-aware routing lives in
// framework/tenantstore.TenantConfigRegistry which selects the right store
// based on schemas.BifrostContextKeyTenantID in the request context.
package configstore
