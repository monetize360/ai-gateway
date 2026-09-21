package tables

// TableAccount maps MPilot billing Account (account__m). The gateway reads it to walk the
// account hierarchy (child → parent) for BudgetUsage checks scoped by account_id, and to
// resolve billingAccountRef (external_id) for InferenceUsage Kafka publish.
type TableAccount struct {
	ID                     string  `gorm:"primaryKey;type:uuid" json:"id"`
	ExternalID             *string `gorm:"column:external_id;type:varchar(255);index" json:"external_id,omitempty"`
	CustomerOrganizationID *string `gorm:"column:customer_organization_id;type:uuid;index" json:"customer_organization_id,omitempty"`
	ParentAccount          *string `gorm:"column:parent_account;type:uuid;index" json:"parent_account,omitempty"`
	IsPrepaid              bool    `gorm:"column:is_prepaid;default:false" json:"is_prepaid"`

	SystemColumns
}

// TableName sets the table name for Account.
func (TableAccount) TableName() string { return "account__m" }
