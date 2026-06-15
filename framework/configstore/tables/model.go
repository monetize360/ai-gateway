package tables

// TableModel represents a model configuration in the database
type TableModel struct {
	ID         string `gorm:"primaryKey;type:uuid" json:"id"`
	ProviderID string `gorm:"type:uuid;index;not null;uniqueIndex:idx_provider_name" json:"provider_id"`
	Name       string `gorm:"uniqueIndex:idx_provider_name" json:"name"`

	InputCostPerToken  *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken *float64 `json:"output_cost_per_token,omitempty"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableModel) TableName() string { return "config_models" }
