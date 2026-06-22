package tables

// TableModelConfig is the API/config.json view of per-model governance limits.
// Rows are stored in config_models (TableModel); budgets and rate limits attach via model_config_id.
type TableModelConfig struct {
	ID        string  `json:"id"`
	ModelName string  `json:"model_name"`
	Provider  *string `json:"provider,omitempty"`

	Budgets    []TableBudget    `json:"budgets,omitempty"`
	RateLimits []TableRateLimit `json:"rate_limits,omitempty"`
	ConfigHash string           `json:"config_hash,omitempty"`

	SystemColumns
}

// ModelConfigFromTableModel builds the governance API shape from a config_models row.
func ModelConfigFromTableModel(m *TableModel, providerName *string) *TableModelConfig {
	if m == nil {
		return nil
	}
	return &TableModelConfig{
		ID:           m.ID,
		ModelName:    m.Name,
		Provider:     providerName,
		Budgets:      m.Budgets,
		RateLimits:   m.RateLimits,
		ConfigHash:   m.ConfigHash,
		SystemColumns: m.SystemColumns,
	}
}

// ApplyModelConfigToTableModel copies governance fields from the API shape onto a config_models row.
func ApplyModelConfigToTableModel(mc *TableModelConfig, row *TableModel) {
	if mc == nil || row == nil {
		return
	}
	if mc.ID != "" {
		row.ID = mc.ID
	}
	if mc.ModelName != "" {
		row.Name = mc.ModelName
	}
	row.Budgets = mc.Budgets
	row.RateLimits = mc.RateLimits
	row.ConfigHash = mc.ConfigHash
	row.SystemColumns = mc.SystemColumns
}
