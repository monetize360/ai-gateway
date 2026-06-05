package tables

// TableCustomer represents a customer entity with budget and rate limit
type TableCustomer struct {
	ID          string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name        string  `gorm:"type:varchar(255);not null" json:"name"`
	BudgetID    *string `gorm:"type:uuid;index" json:"budget_id,omitempty"`
	RateLimitID *string `gorm:"type:uuid;index" json:"rate_limit_id,omitempty"`

	Budget      *TableBudget      `gorm:"foreignKey:BudgetID" json:"budget,omitempty"`
	RateLimit   *TableRateLimit   `gorm:"foreignKey:RateLimitID" json:"rate_limit,omitempty"`
	Teams       []TableTeam       `gorm:"foreignKey:CustomerID" json:"teams"`
	VirtualKeys []TableVirtualKey `gorm:"foreignKey:CustomerID" json:"virtual_keys"`

	ConfigHash string `gorm:"type:varchar(255);null" json:"config_hash"`

	SystemColumns
}

// TableName sets the table name for each model
func (TableCustomer) TableName() string { return "governance_customers" }
