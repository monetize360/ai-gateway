package tables

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// TableOrgLimit holds per-organization budget and rate-limit references.
// Each org may have at most one row (unique org_id). Limits apply to the org and
// are inherited upward through the parent chain when evaluating virtual keys.
type TableOrgLimit struct {
	ID              string  `gorm:"primaryKey;type:uuid" json:"id"`
	OrgID           string  `gorm:"type:uuid;not null;uniqueIndex:idx_governance_org_limits_org_id" json:"org_id"`
	BudgetID        *string `gorm:"type:uuid;index" json:"budget_id,omitempty"`
	RateLimitID     *string `gorm:"type:uuid;index" json:"rate_limit_id,omitempty"`
	CalendarAligned bool    `gorm:"default:false" json:"calendar_aligned"`

	Budget    *TableBudget    `gorm:"foreignKey:BudgetID" json:"budget,omitempty"`
	RateLimit *TableRateLimit `gorm:"foreignKey:RateLimitID" json:"rate_limit,omitempty"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

func (TableOrgLimit) TableName() string { return "governance_org_limits" }

// BeforeSave validates that each org limit row is tied to exactly one organization.
func (ol *TableOrgLimit) BeforeSave(tx *gorm.DB) error {
	if strings.TrimSpace(ol.OrgID) == "" {
		return fmt.Errorf("org_id is required")
	}
	return nil
}

// AfterFind propagates calendar_aligned down to owned budget and rate limit.
func (ol *TableOrgLimit) AfterFind(tx *gorm.DB) error {
	if ol.Budget != nil {
		ol.Budget.IsCalendarAligned = ol.CalendarAligned
	}
	if ol.RateLimit != nil {
		ol.RateLimit.IsCalendarAligned = ol.CalendarAligned
	}
	return nil
}
