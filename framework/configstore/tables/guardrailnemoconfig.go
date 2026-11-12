package tables

// TableGuardrailNemoConfig is a tenant-authored NeMo Colang folder.
// The gateway writes it to the NeMo config directory. MPilot owns the row.
type TableGuardrailNemoConfig struct {
	ID        string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name      string  `gorm:"type:varchar(255);not null" json:"name"`
	ConfigYML string  `gorm:"column:config_yml;type:text" json:"config_yml,omitempty"`
	RailsCo   string  `gorm:"column:rails_co;type:text" json:"rails_co,omitempty"`
	Enabled   *bool   `gorm:"default:true" json:"enabled,omitempty"`
	OrgID     *string `gorm:"column:org_id;type:uuid" json:"org_id,omitempty"`

	SystemColumns
}

func (TableGuardrailNemoConfig) TableName() string { return "guardrail_nemo_configs" }
