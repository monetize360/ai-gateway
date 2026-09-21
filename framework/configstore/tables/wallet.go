package tables

// TableWallet maps MPilot billing Wallet (wallet__m). The gateway reads it for PreLLM
// prepaid-account funds checks. Rating owns balance writes.
type TableWallet struct {
	ID                string   `gorm:"primaryKey;type:uuid" json:"id"`
	AccountID         *string  `gorm:"column:account_id;type:uuid;index" json:"account_id,omitempty"`
	ServiceID         *string  `gorm:"column:service_id;type:uuid;index" json:"service_id,omitempty"`
	Balance           float64  `gorm:"column:balance;default:0" json:"balance"`
	AvailableBalance  *float64 `gorm:"column:available_balance" json:"available_balance,omitempty"`
	IsActive          *bool    `gorm:"column:is_active;default:true" json:"is_active,omitempty"`
	ApprovalStatus    *string  `gorm:"column:approval_status;type:uuid" json:"approval_status,omitempty"`

	SystemColumns
}

// TableName sets the table name for Wallet.
func (TableWallet) TableName() string { return "wallet__m" }
