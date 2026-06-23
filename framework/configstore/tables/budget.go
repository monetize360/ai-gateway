package tables

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// TableBudget defines spending limits with configurable reset periods.
// Parent ownership is expressed via FK columns on this row (virtual_key_id,
// provider_config_id, provider_id, model_config_id, team_id, governed_organization_id).
// org_id is a visibility column (tenant scoping); governed_organization_id links
// the budget to an organization for hierarchy governance checks.
type TableBudget struct {
	ID            string    `gorm:"primaryKey;type:uuid" json:"id"`
	MaxLimit      float64   `gorm:"not null" json:"max_limit"`
	ResetDuration string    `gorm:"type:varchar(50);not null" json:"reset_duration"`
	LastReset     time.Time `gorm:"index" json:"last_reset"`
	CurrentUsage  float64   `gorm:"default:0" json:"current_usage"`

	VirtualKeyID     *string `gorm:"type:uuid;index" json:"virtual_key_id,omitempty"`
	ProviderConfigID *string `gorm:"type:uuid;index" json:"provider_config_id,omitempty"`
	TeamID           *string `gorm:"type:uuid;index" json:"team_id,omitempty"`
	ProviderID       *string `gorm:"type:uuid;index" json:"provider_id,omitempty"`
	ModelConfigID    *string `gorm:"type:uuid;index" json:"model_config_id,omitempty"`
	OrgID                    *string `gorm:"type:uuid;index" json:"org_id,omitempty"`
	GovernedOrganizationID   *string `gorm:"type:uuid;index" json:"governed_organization_id,omitempty"`

	SoftLimit *bool `gorm:"default:false" json:"soft_limit,omitempty"`

	CalendarAlignedInput *bool `gorm:"-" json:"calendar_aligned,omitempty"`
	IsCalendarAligned    bool  `gorm:"-" json:"-"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableBudget) TableName() string { return "governance_budgets" }

// BeforeSave hook for Budget to validate reset duration format and max limit
func (b *TableBudget) BeforeSave(tx *gorm.DB) error {
	if err := validateBudgetOwner(b); err != nil {
		return err
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
