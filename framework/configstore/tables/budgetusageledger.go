package tables

import "time"

// TableBudgetUsageLedger records a snapshot of each BudgetUsage row at the moment it
// resets, providing an audit trail of spend per period. Append-only — rows are never
// updated or soft-deleted.
type TableBudgetUsageLedger struct {
	ID            string    `gorm:"primaryKey;type:uuid" json:"id"`
	BudgetID      string    `gorm:"type:uuid;not null;index" json:"budget_id"`
	Usage         float64   `gorm:"not null" json:"usage"`
	MaxLimit      float64   `gorm:"not null" json:"max_limit"`
	ResetDuration string    `gorm:"type:varchar(50);not null" json:"reset_duration"`
	PeriodStart   time.Time `gorm:"not null" json:"period_start"`
	PeriodEnd     time.Time `gorm:"not null" json:"period_end"`
	CreatedAt     time.Time `gorm:"not null" json:"created_at"`
}

func (TableBudgetUsageLedger) TableName() string { return "budgetusage_ledger" }
