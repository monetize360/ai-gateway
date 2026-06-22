package tables

// TableModel represents a provider model row in config_models (catalog + governance).
type TableModel struct {
	ID         string `gorm:"primaryKey;type:uuid" json:"id"`
	ProviderID string `gorm:"type:uuid;index;not null;uniqueIndex:idx_provider_name" json:"provider_id"`
	Name       string `gorm:"column:name;uniqueIndex:idx_provider_name" json:"name"`

	InputCostPerToken  *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken *float64 `json:"output_cost_per_token,omitempty"`

	Budgets    []TableBudget    `gorm:"foreignKey:ModelConfigID;references:ID" json:"budgets,omitempty"`
	RateLimits []TableRateLimit `gorm:"foreignKey:ModelConfigID;references:ID" json:"rate_limits,omitempty"`
	ConfigHash string           `gorm:"type:varchar(255);null" json:"config_hash,omitempty"`

	// ProviderName is populated during in-memory hydration when only the provider key is known.
	ProviderName string `gorm:"-" json:"-"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableModel) TableName() string { return "config_models" }
