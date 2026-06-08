package lib

import (
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

// NewTestConfig returns a Config that resolves store operations to the given store.
// For unit tests only.
func NewTestConfig(store configstore.ConfigStore) *Config {
	return &Config{
		TenantStore: &TenantStoreHolder{
			Registry: tenantstore.NewStaticRegistry(store, ""),
		},
	}
}
