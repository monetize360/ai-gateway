package tables

// TableUserOrgUnit maps users.id → users.org_unit_id for PreLLM org-unit resolution
// and InferenceUsage orgUnitId stamping. Rating owns no writes on this mapping.
type TableUserOrgUnit struct {
	ID        string  `gorm:"primaryKey;type:uuid;column:id" json:"id"`
	OrgUnitID *string `gorm:"column:org_unit_id;type:uuid;index" json:"org_unit_id,omitempty"`

	SystemColumns
}

func (TableUserOrgUnit) TableName() string { return "users" }
