package tables

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// TableRateLimit defines rate limiting rules using flexible max+reset approach.
// Parent ownership is expressed via FK columns on this row.
type TableRateLimit struct {
	ID string `gorm:"primaryKey;type:uuid" json:"id"`

	TokenMaxLimit      *int64    `gorm:"default:null" json:"token_max_limit,omitempty"`
	TokenResetDuration *string   `gorm:"type:varchar(50)" json:"token_reset_duration,omitempty"`
	TokenCurrentUsage  int64     `gorm:"default:0" json:"token_current_usage"`
	TokenLastReset     time.Time `gorm:"index" json:"token_last_reset"`

	RequestMaxLimit      *int64    `gorm:"default:null" json:"request_max_limit,omitempty"`
	RequestResetDuration *string   `gorm:"type:varchar(50)" json:"request_reset_duration,omitempty"`
	RequestCurrentUsage  int64     `gorm:"default:0" json:"request_current_usage"`
	RequestLastReset     time.Time `gorm:"index" json:"request_last_reset"`

	VirtualKeyID     *string `gorm:"type:uuid;index" json:"virtual_key_id,omitempty"`
	ProviderConfigID *string `gorm:"type:uuid;index" json:"provider_config_id,omitempty"`
	ProviderID       *string `gorm:"type:uuid;index" json:"provider_id,omitempty"`
	ModelConfigID    *string `gorm:"type:uuid;index" json:"model_config_id,omitempty"`
	OrgID                    *string `gorm:"type:uuid;index" json:"org_id,omitempty"`
	GovernedOrganizationID   *string `gorm:"type:uuid;index" json:"governed_organization_id,omitempty"`

	// SoftLimit when true allows requests to proceed even when this rate limit is exceeded.
	// Usage is still tracked and tokens_used/request percentage is still reported.
	// Pair with a routing rule using `tokens_used >= 100.0` or `request >= 100.0` to switch
	// models on exhaustion without blocking requests.
	SoftLimit bool `gorm:"not null;default:false" json:"soft_limit,omitempty"`

	CalendarAlignedInput *bool `gorm:"-" json:"calendar_aligned,omitempty"`
	IsCalendarAligned    bool  `gorm:"-" json:"-"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableRateLimit) TableName() string { return "governance_rate_limits" }

// BeforeSave hook for RateLimit to validate reset duration formats
func (rl *TableRateLimit) BeforeSave(tx *gorm.DB) error {
	if err := validateRateLimitOwner(rl); err != nil {
		return err
	}
	if rl.TokenResetDuration != nil {
		if d, err := ParseDuration(*rl.TokenResetDuration); err != nil {
			return fmt.Errorf("invalid token reset duration format: %s", *rl.TokenResetDuration)
		} else if d <= 0 {
			return fmt.Errorf("token reset duration cannot be zero or negative: %s", *rl.TokenResetDuration)
		}
	}
	if rl.RequestResetDuration != nil {
		if d, err := ParseDuration(*rl.RequestResetDuration); err != nil {
			return fmt.Errorf("invalid request reset duration format: %s", *rl.RequestResetDuration)
		} else if d <= 0 {
			return fmt.Errorf("request reset duration cannot be zero or negative: %s", *rl.RequestResetDuration)
		}
	}
	if rl.TokenMaxLimit != nil && rl.TokenResetDuration == nil {
		return fmt.Errorf("token_reset_duration is required when token_max_limit is set")
	}
	if rl.RequestMaxLimit != nil && rl.RequestResetDuration == nil {
		return fmt.Errorf("request_reset_duration is required when request_max_limit is set")
	}
	if rl.TokenMaxLimit != nil && *rl.TokenMaxLimit <= 0 {
		return fmt.Errorf("token_max_limit cannot be zero or negative: %d", *rl.TokenMaxLimit)
	}
	if rl.RequestMaxLimit != nil && *rl.RequestMaxLimit <= 0 {
		return fmt.Errorf("request_max_limit cannot be zero or negative: %d", *rl.RequestMaxLimit)
	}
	return nil
}
