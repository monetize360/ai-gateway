package tables

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// TableModelConfig represents a model configuration with rate limiting and budgeting
type TableModelConfig struct {
	ID          string  `gorm:"primaryKey;type:uuid" json:"id"`
	ModelName   string  `gorm:"type:varchar(255);not null;uniqueIndex:idx_model_provider" json:"model_name"`
	Provider    *string `gorm:"type:varchar(50);uniqueIndex:idx_model_provider" json:"provider,omitempty"`
	BudgetID    *string `gorm:"type:uuid;index:idx_model_config_budget" json:"budget_id,omitempty"`
	RateLimitID *string `gorm:"type:uuid;index:idx_model_config_rate_limit" json:"rate_limit_id,omitempty"`

	Budget    *TableBudget    `gorm:"foreignKey:BudgetID;onDelete:CASCADE" json:"budget,omitempty"`
	RateLimit *TableRateLimit `gorm:"foreignKey:RateLimitID;onDelete:CASCADE" json:"rate_limit,omitempty"`

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
	if mc.BudgetID != nil && strings.TrimSpace(*mc.BudgetID) == "" {
		return fmt.Errorf("budget_id cannot be an empty string")
	}
	if mc.RateLimitID != nil && strings.TrimSpace(*mc.RateLimitID) == "" {
		return fmt.Errorf("rate_limit_id cannot be an empty string")
	}
	if mc.Provider != nil && strings.TrimSpace(*mc.Provider) == "" {
		return fmt.Errorf("provider cannot be an empty string")
	}
	return nil
}
