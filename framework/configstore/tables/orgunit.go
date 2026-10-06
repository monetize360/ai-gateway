package tables

// TableOrgUnit maps MPilot OrgUnit (orgunit__m). The gateway reads it to walk
// parent_id (leaf → root) for BudgetUsage checks scoped by org_unit_id.
type TableOrgUnit struct {
	ID       string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name     string  `gorm:"column:name;type:varchar(255)" json:"name"`
	ParentID *string `gorm:"column:parent_id;type:uuid;index" json:"parent_id,omitempty"`

	SystemColumns
}

func (TableOrgUnit) TableName() string { return "orgunit__m" }
