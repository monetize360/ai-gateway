package tables

import "time"

// TableBudgetUsage maps MPilot billing BudgetUsage (budgetusage__m).
// Used by PreLLM for service / provider / account / contract / user spending limits.
// CurrentUsage is owned by MPilot Rating; the gateway reads and caches rows, and only
// writes the periodic reset (current_usage=0 + last_reset) along with a ledger entry.
type TableBudgetUsage struct {
	ID           string  `gorm:"primaryKey;type:uuid" json:"id"`
	CurrentUsage float64 `gorm:"column:current_usage;default:0" json:"current_usage"`
	MaxLimit     float64 `gorm:"column:max_limit;not null" json:"max_limit"`

	// ResetDuration is the rolling window for the limit ("1h", "1d", "1M"); empty means never resets.
	ResetDuration string     `gorm:"column:reset_duration;type:varchar(50)" json:"reset_duration,omitempty"`
	LastReset     *time.Time `gorm:"column:last_reset" json:"last_reset,omitempty"`
	// SoftLimit allows requests through once the limit is crossed; usage is still tracked
	// and routing rules can react via soft_limit_exceeded.
	SoftLimit bool `gorm:"column:soft_limit;default:false" json:"soft_limit"`

	// Owner scopes — at most one is set per row.
	ServiceID  *string `gorm:"column:service_id;type:uuid;index" json:"service_id,omitempty"`
	ProviderID *string `gorm:"column:provider_id;type:uuid;index" json:"provider_id,omitempty"`
	AccountID  *string `gorm:"column:account_id;type:uuid;index" json:"account_id,omitempty"`
	ContractID *string `gorm:"column:contract_id;type:uuid;index" json:"contract_id,omitempty"`
	UserID     *string `gorm:"column:user_id;type:uuid;index" json:"user_id,omitempty"`
	OrgUnitID  *string `gorm:"column:org_unit_id;type:uuid;index" json:"org_unit_id,omitempty"`

	SystemColumns
}

// TableName sets the table name for BudgetUsage.
func (TableBudgetUsage) TableName() string { return "budgetusage__m" }
