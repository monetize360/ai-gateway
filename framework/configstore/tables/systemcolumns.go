package tables

import "time"

// SystemColumns holds MPilot-compatible audit and soft-delete fields shared by
// governance_* tables. Platform code auto-manages these; they must not appear
// in config.json governance payloads.
type SystemColumns struct {
	CreatedBy *string   `gorm:"type:uuid" json:"created_by,omitempty"`
	UpdatedBy *string   `gorm:"type:uuid" json:"updated_by,omitempty"`
	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"index;not null" json:"updated_at"`
	Deleted   bool      `gorm:"not null;default:false;index" json:"-"`
}
