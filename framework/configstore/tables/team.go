package tables

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// TableTeam represents a team entity with budget, rate limit and customer association
type TableTeam struct {
	ID          string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name        string  `gorm:"type:varchar(255);not null;uniqueIndex" json:"name"`
	CustomerID  *string `gorm:"type:uuid;index" json:"customer_id,omitempty"`
	RateLimitID *string `gorm:"type:uuid;index" json:"rate_limit_id,omitempty"`
	SourceID    *string `gorm:"type:varchar(255);uniqueIndex" json:"source_id,omitempty"`

	Customer    *TableCustomer    `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	Budgets     []TableBudget     `gorm:"foreignKey:TeamID;constraint:OnDelete:CASCADE" json:"budgets,omitempty"`
	RateLimit   *TableRateLimit   `gorm:"foreignKey:RateLimitID" json:"rate_limit,omitempty"`
	VirtualKeys []TableVirtualKey `gorm:"foreignKey:TeamID" json:"virtual_keys,omitempty"`

	VirtualKeyCount int64 `gorm:"->;-:migration" json:"virtual_key_count"`

	Profile       *string        `gorm:"type:text" json:"-"`
	ParsedProfile map[string]any `gorm:"-" json:"profile"`

	Config       *string        `gorm:"type:text" json:"-"`
	ParsedConfig map[string]any `gorm:"-" json:"config"`

	Claims       *string        `gorm:"type:text" json:"-"`
	ParsedClaims map[string]any `gorm:"-" json:"claims"`

	CalendarAligned bool `gorm:"default:false" json:"calendar_aligned"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableTeam) TableName() string { return "governance_teams" }

// BeforeSave hook for TableTeam to serialize JSON fields
func (t *TableTeam) BeforeSave(tx *gorm.DB) error {
	if t.SourceID != nil {
		v := strings.TrimSpace(*t.SourceID)
		if v == "" {
			t.SourceID = nil
		} else {
			*t.SourceID = v
		}
	}
	if t.ParsedProfile != nil {
		data, err := json.Marshal(t.ParsedProfile)
		if err != nil {
			return err
		}
		t.Profile = new(string(data))
	} else {
		t.Profile = nil
	}
	if t.ParsedConfig != nil {
		data, err := json.Marshal(t.ParsedConfig)
		if err != nil {
			return err
		}
		t.Config = new(string(data))
	} else {
		t.Config = nil
	}
	if t.ParsedClaims != nil {
		data, err := json.Marshal(t.ParsedClaims)
		if err != nil {
			return err
		}
		t.Claims = new(string(data))
	} else {
		t.Claims = nil
	}
	return nil
}

// AfterFind hook for TableTeam to deserialize JSON fields and propagate
// calendar_aligned down to owned budgets / rate_limit.
func (t *TableTeam) AfterFind(tx *gorm.DB) error {
	if t.Profile != nil {
		if err := json.Unmarshal([]byte(*t.Profile), &t.ParsedProfile); err != nil {
			return err
		}
	}
	if t.Config != nil {
		if err := json.Unmarshal([]byte(*t.Config), &t.ParsedConfig); err != nil {
			return err
		}
	}
	if t.Claims != nil {
		if err := json.Unmarshal([]byte(*t.Claims), &t.ParsedClaims); err != nil {
			return err
		}
	}
	for i := range t.Budgets {
		t.Budgets[i].IsCalendarAligned = t.CalendarAligned
	}
	if t.RateLimit != nil {
		t.RateLimit.IsCalendarAligned = t.CalendarAligned
	}
	return nil
}
