package tables

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// TableModelConfig represents a model configuration with rate limiting and budgeting.
// Budget and rate limit ownership lives on the child rows via model_config_id.
type TableModelConfig struct {
	ID        string  `gorm:"primaryKey;type:uuid" json:"id"`
	ModelName string  `gorm:"type:varchar(255);not null;uniqueIndex:idx_model_provider" json:"model_name"`
	Provider  *string `gorm:"type:varchar(50);uniqueIndex:idx_model_provider" json:"provider,omitempty"`

	Budgets    []TableBudget    `gorm:"foreignKey:ModelConfigID;references:ID" json:"budgets,omitempty"`
	RateLimits []TableRateLimit `gorm:"foreignKey:ModelConfigID;references:ID" json:"rate_limits,omitempty"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableModelConfig) TableName() string {
	return "governance_model_configs"
}

// BeforeSave hook for ModelConfig to validate required fields
func (mc *TableModelConfig) BeforeSave(tx *gorm.DB) error {
	if strings.TrimSpace(mc.ModelName) == "" {
		return fmt.Errorf("model_name cannot be empty")
	}
	if mc.Provider != nil && strings.TrimSpace(*mc.Provider) == "" {
		return fmt.Errorf("provider cannot be an empty string")
	}
	return nil
}
