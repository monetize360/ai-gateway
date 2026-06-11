package configstore

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// scopeProviderByRuntimeKey scopes a config_providers query by provider_type (standard)
// or provider_type+custom name (custom providers).
func scopeProviderByRuntimeKey(db *gorm.DB, provider schemas.ModelProvider) *gorm.DB {
	key := strings.TrimSpace(string(provider))
	if picklistID, ok := tables.PicklistItemIDForProviderName(key); ok {
		return db.Where("provider_type = ?", picklistID)
	}
	return db.Where("provider_type = ? AND name = ?", tables.CustomProviderPicklistItemID, key)
}

// scopeJoinedProviderByRuntimeKey scopes a joined config_providers query by runtime provider key.
func scopeJoinedProviderByRuntimeKey(db *gorm.DB, provider schemas.ModelProvider) *gorm.DB {
	key := strings.TrimSpace(string(provider))
	if picklistID, ok := tables.PicklistItemIDForProviderName(key); ok {
		return db.Where("config_providers.deleted = ? AND config_providers.provider_type = ?", false, picklistID)
	}
	return db.Where(
		"config_providers.deleted = ? AND config_providers.provider_type = ? AND config_providers.name = ?",
		false, tables.CustomProviderPicklistItemID, key,
	)
}
