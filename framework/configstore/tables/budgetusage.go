package tables

// TableBudgetUsage maps MPilot billing BudgetUsage (budgetusage__m).
// Used by ai_infra PreLLM for account / contract / user spending limits.
// Usage is owned by MPilot Rating; the gateway only reads and caches rows.
type TableBudgetUsage struct {
	ID           string   `gorm:"primaryKey;type:uuid" json:"id"`
	CurrentUsage float64  `gorm:"column:current_usage;default:0" json:"current_usage"`
	MaxLimit     float64  `gorm:"column:max_limit;not null" json:"max_limit"`
	AccountID    *string  `gorm:"column:account_id;type:uuid;index" json:"account_id,omitempty"`
	ContractID   *string  `gorm:"column:contract_id;type:uuid;index" json:"contract_id,omitempty"`
	UserID       *string  `gorm:"column:user_id;type:uuid;index" json:"user_id,omitempty"`

	SystemColumns
}

// TableName sets the table name for BudgetUsage.
func (TableBudgetUsage) TableName() string { return "budgetusage__m" }
