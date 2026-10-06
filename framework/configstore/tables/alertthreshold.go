package tables

// Alert target and metric values stored on alertthreshold__m.
const (
	AlertTargetBudgetUsage      = "BUDGET_USAGE"
	AlertTargetWallet           = "WALLET"
	AlertMetricUsagePercent     = "USAGE_PERCENT"
	AlertMetricUsageAmount      = "USAGE_AMOUNT"
	AlertMetricAvailableBalance = "AVAILABLE_BALANCE"
	WalletAlertPeriodKey        = "open"
)

// TableAlertThreshold is a budget or wallet line edited in MPilot and cached by the gateway.
type TableAlertThreshold struct {
	ID               string   `gorm:"primaryKey;type:uuid" json:"id"`
	Name             string   `gorm:"column:name;type:varchar(255)" json:"name,omitempty"`
	TargetType       string   `gorm:"column:target_type;type:varchar(50)" json:"target_type"`
	TargetID         string   `gorm:"column:target_id;type:uuid;index" json:"target_id"`
	Metric           string   `gorm:"column:metric;type:varchar(50)" json:"metric"`
	ThresholdValue   float64  `gorm:"column:threshold_value" json:"threshold_value"`
	Enabled          *bool    `gorm:"column:enabled" json:"enabled,omitempty"`
	RecipientRoleIDs []string `gorm:"column:recipient_role_ids;type:jsonb;serializer:json" json:"recipient_role_ids,omitempty"`
	RecipientUserIDs []string `gorm:"column:recipient_user_ids;type:jsonb;serializer:json" json:"recipient_user_ids,omitempty"`

	SystemColumns
}

// TableName sets the table name for AlertThreshold.
func (TableAlertThreshold) TableName() string { return "alertthreshold__m" }

// AlertThresholdEnabled reports whether the gateway should evaluate this row.
// A null enabled flag is treated as on so a row is not silently skipped.
func AlertThresholdEnabled(row *TableAlertThreshold) bool {
	if row == nil || row.Deleted {
		return false
	}
	return row.Enabled == nil || *row.Enabled
}
