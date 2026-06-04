// Package configstore — tenant-aware store helpers.
//
// Per-tenant ConfigStore instances are created by
// NewPostgresConfigStoreFromDSN (see postgres.go), which accepts a pre-built
// connection string rather than the full PostgresConfig struct. This is used
// by framework/tenantstore.TenantDBManager to lazily initialise one store per
// tenant on first access.
//
// Higher-level context-aware routing lives in
// framework/tenantstore.TenantConfigRegistry which selects the right store
// based on schemas.BifrostContextKeyTenantID in the request context.
package configstore
