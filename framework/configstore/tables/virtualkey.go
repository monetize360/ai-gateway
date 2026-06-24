package tables

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/gorm"
)

// TableAllowedModelConfigKey is the join table for the many2many relationship
// between TableAllowedModelConfig and TableKey.
type TableAllowedModelConfigKey struct {
	ID                        string `gorm:"primaryKey;type:uuid" json:"id"`
	TableAllowedModelConfigID string `gorm:"type:uuid;not null;uniqueIndex:idx_vk_provider_config_key;column:table_virtual_key_provider_config_id" json:"table_virtual_key_provider_config_id"`
	TableKeyID                string `gorm:"type:uuid;not null;uniqueIndex:idx_vk_provider_config_key" json:"table_key_id"`

	SystemColumns
}

// TableName sets the table name for the join table
func (TableAllowedModelConfigKey) TableName() string {
	return "governance_virtual_key_provider_config_keys"
}

// TableAllowedModelConfig represents a per-provider model allow/block configuration
// scoped to a virtual key (VirtualKeyID set) or an organization (ScopeOrgID set).
type TableAllowedModelConfig struct {
	ID           string  `gorm:"primaryKey;type:uuid" json:"id"`
	VirtualKeyID *string `gorm:"type:uuid" json:"virtual_key_id,omitempty"`
	ScopeOrgID   *string `gorm:"type:uuid;index" json:"scope_org_id,omitempty"`
	Provider     string  `gorm:"type:varchar(50);not null" json:"provider"`
	Weight       *float64          `json:"weight"`
	AllowedModels     schemas.WhiteList `gorm:"type:text;serializer:json" json:"allowed_models"`
	BlacklistedModels schemas.BlackList `gorm:"type:text;serializer:json" json:"blacklisted_models"`
	AllowAllKeys      bool              `gorm:"default:false" json:"allow_all_keys"`

	// Relationships — budget/rate limit FK columns live on child rows
	RateLimits []TableRateLimit `gorm:"foreignKey:ProviderConfigID;references:ID" json:"rate_limits,omitempty"`
	Budgets    []TableBudget    `gorm:"foreignKey:ProviderConfigID;constraint:OnDelete:CASCADE" json:"budgets,omitempty"`
	Keys       []TableKey       `gorm:"many2many:governance_virtual_key_provider_config_keys;constraint:OnDelete:CASCADE" json:"keys"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableAllowedModelConfig) TableName() string {
	return "governance_virtual_key_provider_configs"
}

// UnmarshalJSON custom unmarshaller to handle "key_ids" ([]string) config-file format
func (pc *TableAllowedModelConfig) UnmarshalJSON(data []byte) error {
	type Alias TableAllowedModelConfig
	type TempProviderConfig struct {
		Alias
		KeyIDs []string `json:"key_ids"` // Config file format: key identifiers (TableKey.KeyID); use ["*"] to allow all keys, empty denies all
	}

	var temp TempProviderConfig
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}

	// Copy all standard fields
	*pc = TableAllowedModelConfig(temp.Alias)

	// If key_ids is provided, convert to Keys or set AllowAllKeys
	if len(temp.KeyIDs) > 0 && len(pc.Keys) == 0 {
		// ["*"] means allow all keys
		if len(temp.KeyIDs) == 1 && temp.KeyIDs[0] == "*" {
			pc.AllowAllKeys = true
			pc.Keys = nil
		} else {
			pc.AllowAllKeys = false
			pc.Keys = make([]TableKey, len(temp.KeyIDs))
			for i, keyID := range temp.KeyIDs {
				pc.Keys[i] = TableKey{KeyID: keyID}
			}
		}
	}

	return nil
}

// BeforeSave validates scope association and list fields before GORM persists the record.
func (pc *TableAllowedModelConfig) BeforeSave(tx *gorm.DB) error {
	if err := pc.AllowedModels.Validate(); err != nil {
		return fmt.Errorf("invalid allowed_models: %w", err)
	}
	if err := pc.BlacklistedModels.Validate(); err != nil {
		return fmt.Errorf("invalid blacklisted_models: %w", err)
	}
	vkSet := isNonEmptyString(pc.VirtualKeyID)
	orgSet := isNonEmptyString(pc.ScopeOrgID)
	if vkSet && orgSet {
		return fmt.Errorf("virtual_key_id and scope_org_id are mutually exclusive")
	}
	if !vkSet && !orgSet {
		return fmt.Errorf("either virtual_key_id or scope_org_id must be set")
	}
	return nil
}

// MarshalJSON custom marshaller to ensure AllowedModels and BlacklistedModels are always arrays (never null)
func (pc TableAllowedModelConfig) MarshalJSON() ([]byte, error) {
	type Alias TableAllowedModelConfig

	// Ensure arrays are empty slices instead of nil
	allowedModels := pc.AllowedModels
	if allowedModels == nil {
		allowedModels = []string{}
	}
	blacklistedModels := pc.BlacklistedModels
	if blacklistedModels == nil {
		blacklistedModels = []string{}
	}

	return json.Marshal(&struct {
		Alias
		AllowedModels     []string `json:"allowed_models"`
		BlacklistedModels []string `json:"blacklisted_models"`
	}{
		Alias:             Alias(pc),
		AllowedModels:     allowedModels,
		BlacklistedModels: blacklistedModels,
	})
}

// AfterFind hook for TableAllowedModelConfig to clear sensitive data from associated keys
func (pc *TableAllowedModelConfig) AfterFind(tx *gorm.DB) error {
	if pc.Keys != nil {
		// Clear sensitive data from associated keys, keeping only key IDs and non-sensitive metadata
		for i := range pc.Keys {
			key := &pc.Keys[i]

			// Clear the actual API key value
			key.Value = *schemas.NewEnvVar("")

			// Clear all Azure-related sensitive fields
			key.AzureEndpoint = nil
			key.AzureClientID = nil
			key.AzureClientSecret = nil
			key.AzureTenantID = nil
			key.AzureScopesJSON = nil
			key.AzureKeyConfig = nil

			// Clear all Vertex-related sensitive fields
			key.VertexProjectID = nil
			key.VertexProjectNumber = nil
			key.VertexRegion = nil
			key.VertexAuthCredentials = nil
			key.VertexKeyConfig = nil

			// Clear all Bedrock-related sensitive fields
			key.BedrockAccessKey = nil
			key.BedrockSecretKey = nil
			key.BedrockSessionToken = nil
			key.BedrockRegion = nil
			key.BedrockARN = nil
			key.BedrockRoleARN = nil
			key.BedrockExternalID = nil
			key.BedrockRoleSessionName = nil
			key.BedrockKeyConfig = nil

			pc.Keys[i] = *key
		}
	}
	return nil
}

type TableVirtualKeyMCPConfig struct {
	ID             string            `gorm:"primaryKey;type:uuid" json:"id"`
	VirtualKeyID   string            `gorm:"type:uuid;not null;uniqueIndex:idx_vk_mcpclient" json:"virtual_key_id"`
	MCPClientID    string            `gorm:"type:uuid;not null;uniqueIndex:idx_vk_mcpclient" json:"mcp_client_id"`
	MCPClient      TableMCPClient    `gorm:"foreignKey:MCPClientID" json:"mcp_client"`
	ToolsToExecute schemas.WhiteList `gorm:"type:text;serializer:json" json:"tools_to_execute"`

	MCPClientName string `gorm:"-" json:"-"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableVirtualKeyMCPConfig) TableName() string {
	return "governance_virtual_key_mcp_configs"
}

// BeforeSave validates WhiteList fields before GORM persists the record.
func (mc *TableVirtualKeyMCPConfig) BeforeSave(tx *gorm.DB) error {
	if err := mc.ToolsToExecute.Validate(); err != nil {
		return fmt.Errorf("invalid tools_to_execute: %w", err)
	}
	return nil
}

// UnmarshalJSON custom unmarshaller to handle both "mcp_client_id" (database format)
// and "mcp_client_name" (config file format) for MCP client references.
func (mc *TableVirtualKeyMCPConfig) UnmarshalJSON(data []byte) error {
	// Temporary struct to capture all fields including mcp_client_name
	type Alias TableVirtualKeyMCPConfig
	type TempMCPConfig struct {
		Alias
		MCPClientName string `json:"mcp_client_name"` // Config file format: MCP client name
	}
	var temp TempMCPConfig
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	// Copy all standard fields
	*mc = TableVirtualKeyMCPConfig(temp.Alias)
	// Capture mcp_client_name for later resolution to MCPClientID
	if temp.MCPClientName != "" {
		mc.MCPClientName = temp.MCPClientName
	}
	return nil
}

// TableVirtualKey represents a virtual key with budget, rate limits, and org association
type TableVirtualKey struct {
	ID              string                          `gorm:"primaryKey;type:uuid" json:"id"`
	Name            string                          `gorm:"uniqueIndex:idx_virtual_key_name;type:varchar(255);not null" json:"name"`
	Description     string                          `gorm:"type:text" json:"description,omitempty"`
	Value           string                          `gorm:"uniqueIndex:idx_virtual_key_value;type:text;not null" json:"value"`           // The virtual key value
	IsActive             *bool                       `gorm:"default:true" json:"is_active,omitempty"`                                          // Nil means true (DB default); false means inactive
	AllowedModelConfigs  []TableAllowedModelConfig   `gorm:"foreignKey:VirtualKeyID;constraint:OnDelete:CASCADE" json:"allowed_model_configs"` // VK-scoped; empty means all providers/models allowed
	OrgAllowedModelConfigs []TableAllowedModelConfig `gorm:"-" json:"-"`                                                                       // Org-scoped configs resolved at runtime by the governance store
	MCPConfigs           []TableVirtualKeyMCPConfig  `gorm:"foreignKey:VirtualKeyID;constraint:OnDelete:CASCADE" json:"mcp_configs"`

	// OrgID is reserved for MPilot tenant visibility and is not used by the governance engine.
	OrgID *string `gorm:"type:uuid;index" json:"org_id,omitempty"`
	// ScopeOrgID is the org used for budget, rate limit, and routing scope at runtime.
	// When set, it takes precedence over OrgID.
	ScopeOrgID *string `gorm:"type:uuid;index" json:"scope_org_id,omitempty"`

	CalendarAligned bool `gorm:"default:false" json:"calendar_aligned"`

	// Relationships — budget/rate limit FK columns live on child rows
	RateLimits []TableRateLimit `gorm:"foreignKey:VirtualKeyID;references:ID" json:"rate_limits,omitempty"`
	Budgets    []TableBudget    `gorm:"foreignKey:VirtualKeyID;constraint:OnDelete:CASCADE" json:"budgets,omitempty"`

	// Config hash is used to detect the changes synced from config.json file
	// Every time we sync the config.json file, we will update the config hash
	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	EncryptionStatus string `gorm:"type:varchar(20);default:'plain_text'" json:"-"`
	ValueHash        string `gorm:"type:varchar(64);index:idx_virtual_key_value_hash,unique" json:"-"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableVirtualKey) TableName() string { return "governance_virtual_keys" }

// IsActiveValue returns the effective IsActive bool, treating nil as true (DB default).
func (vk *TableVirtualKey) IsActiveValue() bool {
	if vk == nil {
		return false
	}
	if vk.IsActive == nil {
		return true
	}
	return *vk.IsActive
}

// GovernanceScopeOrgID returns the org ID used for budget, rate limit, and routing scope.
// scope_org_id takes precedence over org_id.
func (vk *TableVirtualKey) GovernanceScopeOrgID() *string {
	if vk == nil {
		return nil
	}
	if isNonEmptyString(vk.ScopeOrgID) {
		return vk.ScopeOrgID
	}
	if isNonEmptyString(vk.OrgID) {
		return vk.OrgID
	}
	return nil
}

// GovernanceScopeOrgIDString returns the trimmed governance scope org ID, or empty if unset.
func (vk *TableVirtualKey) GovernanceScopeOrgIDString() string {
	if scopeOrgID := vk.GovernanceScopeOrgID(); scopeOrgID != nil {
		return strings.TrimSpace(*scopeOrgID)
	}
	return ""
}

// BeforeSave computes a SHA-256 hash of the plaintext value for indexed lookups and
// encrypts the virtual key value before writing to the database.
func (vk *TableVirtualKey) BeforeSave(tx *gorm.DB) error {
	// Hash must be computed before encryption (from plaintext value)
	if vk.Value != "" {
		vk.ValueHash = encrypt.HashSHA256(vk.Value)
	}
	if encrypt.IsEnabled() && vk.Value != "" {
		if err := encryptString(&vk.Value); err != nil {
			return fmt.Errorf("failed to encrypt virtual key value: %w", err)
		}
		vk.EncryptionStatus = EncryptionStatusEncrypted
	}
	return nil
}

// AfterFind is a GORM hook that decrypts the virtual key value after reading
// from the database and propagates VK-level calendar_aligned down to owned
// budgets / rate_limit and to each provider config's budgets / rate_limit.
// The reset path reads the stamped value; Update*InMemory paths re-stamp on
// every VK update.
func (vk *TableVirtualKey) AfterFind(tx *gorm.DB) error {
	if vk.EncryptionStatus == EncryptionStatusEncrypted {
		if err := decryptString(&vk.Value); err != nil {
			return fmt.Errorf("failed to decrypt virtual key value: %w", err)
		}
	}
	for i := range vk.Budgets {
		vk.Budgets[i].IsCalendarAligned = vk.CalendarAligned
	}
	for i := range vk.RateLimits {
		vk.RateLimits[i].IsCalendarAligned = vk.CalendarAligned
	}
	for i := range vk.AllowedModelConfigs {
		pc := &vk.AllowedModelConfigs[i]
		for j := range pc.Budgets {
			pc.Budgets[j].IsCalendarAligned = vk.CalendarAligned
		}
		for j := range pc.RateLimits {
			pc.RateLimits[j].IsCalendarAligned = vk.CalendarAligned
		}
	}
	return nil
}

// Backward-compatible aliases for legacy provider-config naming.
type TableVirtualKeyProviderConfig = TableAllowedModelConfig
type TableVirtualKeyProviderConfigKey = TableAllowedModelConfigKey
