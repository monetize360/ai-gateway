package tables

// TableOrganization mirrors the mpilotv2 `organizations` table in each tenant database.
// Bifrost reads this table read-only to walk the org hierarchy for governance limits.
type TableOrganization struct {
	ID          string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name        string  `gorm:"column:name;type:varchar(255);not null" json:"name"`
	ParentOrgID *string `gorm:"column:parent_org_id;type:uuid;index" json:"parent_org_id,omitempty"`
	IsRoot      bool    `gorm:"column:is_root;not null;default:false" json:"is_root"`

	SystemColumns
}

func (TableOrganization) TableName() string { return "organizations" }
