package tables

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// TableBudget defines spending limits with configurable reset periods
type TableBudget struct {
	ID            string    `gorm:"primaryKey;type:uuid" json:"id"`
	MaxLimit      float64   `gorm:"not null" json:"max_limit"`
	ResetDuration string    `gorm:"type:varchar(50);not null" json:"reset_duration"`
	LastReset     time.Time `gorm:"index" json:"last_reset"`
	CurrentUsage  float64   `gorm:"default:0" json:"current_usage"`

	TeamID           *string `gorm:"type:uuid;index" json:"team_id,omitempty"`
	VirtualKeyID     *string `gorm:"type:uuid;index" json:"virtual_key_id,omitempty"`
	ProviderConfigID *string `gorm:"type:uuid;index" json:"provider_config_id,omitempty"`

	CalendarAlignedInput *bool `gorm:"-" json:"calendar_aligned,omitempty"`
	IsCalendarAligned    bool  `gorm:"-" json:"-"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableBudget) TableName() string { return "governance_budgets" }

// BeforeSave hook for Budget to validate reset duration format and max limit
func (b *TableBudget) BeforeSave(tx *gorm.DB) error {
	owners := 0
	if b.TeamID != nil {
		owners++
	}
	if b.VirtualKeyID != nil {
		owners++
	}
	if b.ProviderConfigID != nil {
		owners++
	}
	if owners > 1 {
		return fmt.Errorf("budget cannot have more than one owner (team/virtual key/provider config)")
	}
	if d, err := ParseDuration(b.ResetDuration); err != nil {
		return fmt.Errorf("invalid reset duration format: %s", b.ResetDuration)
	} else if d <= 0 {
		return fmt.Errorf("reset duration must be > 0: %s", b.ResetDuration)
	}
	if b.MaxLimit < 0 {
		return fmt.Errorf("budget max_limit cannot be negative: %.2f", b.MaxLimit)
	}
	return nil
}
