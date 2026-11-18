package tables

import "strings"

// TableGuardrailPolicy is a tenant-owned guardrail definition (guardrail_policies).
// The gateway reads it; MPilot owns writes.
type TableGuardrailPolicy struct {
	ID                string  `gorm:"primaryKey;type:uuid" json:"id"`
	Name              string  `gorm:"type:varchar(255);not null" json:"name"`
	Description       string  `gorm:"type:text" json:"description,omitempty"`
	Enabled           *bool   `gorm:"default:true" json:"enabled,omitempty"`
	GuardrailType     *string `gorm:"column:guardrail_type;type:uuid" json:"guardrail_type,omitempty"`
	EvaluatorType     *string `gorm:"column:evaluator_type;type:varchar(64)" json:"evaluator_type,omitempty"`
	AllowRequestOptIn *bool   `gorm:"column:allow_request_opt_in" json:"allow_request_opt_in,omitempty"`
	Stage             *string `gorm:"column:stage;type:uuid" json:"stage,omitempty"`
	Action            *string `gorm:"column:action;type:uuid" json:"action,omitempty"`
	StreamMode        *string `gorm:"column:stream_mode;type:uuid" json:"stream_mode,omitempty"`
	ConfigJSON        string  `gorm:"column:config_json;type:text" json:"config_json,omitempty"`
	FailClosed        *bool   `gorm:"column:fail_closed;default:false" json:"fail_closed,omitempty"`
	TimeoutMs         *int    `gorm:"column:timeout_ms" json:"timeout_ms,omitempty"`
	MaxTurnsToSend    *int    `gorm:"column:max_turns_to_send" json:"max_turns_to_send,omitempty"`
	StreamChunkChars  *int    `gorm:"column:stream_chunk_chars" json:"stream_chunk_chars,omitempty"`
	OrgID             *string `gorm:"column:org_id;type:uuid" json:"org_id,omitempty"`

	SystemColumns
}

func (TableGuardrailPolicy) TableName() string { return "guardrail_policies" }

func (p *TableGuardrailPolicy) EnabledValue() bool {
	if p == nil || p.Enabled == nil {
		return p != nil
	}
	return *p.Enabled
}

func (p *TableGuardrailPolicy) FailClosedValue() bool {
	return p != nil && p.FailClosed != nil && *p.FailClosed
}

func (p *TableGuardrailPolicy) TypeName() string {
	if p == nil {
		return ""
	}
	if p.EvaluatorType != nil && strings.TrimSpace(*p.EvaluatorType) != "" {
		return GuardrailTypeName(strings.TrimSpace(*p.EvaluatorType))
	}
	if p.GuardrailType == nil {
		return ""
	}
	return GuardrailTypeName(*p.GuardrailType)
}

func (p *TableGuardrailPolicy) AllowOptInValue() bool {
	return p != nil && p.AllowRequestOptIn != nil && *p.AllowRequestOptIn
}

func (p *TableGuardrailPolicy) StageName() string {
	if p == nil || p.Stage == nil {
		return GuardrailStageBoth
	}
	name := GuardrailStageName(*p.Stage)
	if name == "" {
		return GuardrailStageBoth
	}
	return name
}

func (p *TableGuardrailPolicy) ActionName() string {
	if p == nil || p.Action == nil {
		return GuardrailActionBlock
	}
	name := GuardrailActionName(*p.Action)
	if name == "" {
		return GuardrailActionBlock
	}
	return name
}

func (p *TableGuardrailPolicy) StreamModeName() string {
	if p == nil || p.StreamMode == nil {
		return GuardrailStreamModeChunked
	}
	name := GuardrailStreamModeName(*p.StreamMode)
	if name == "" {
		return GuardrailStreamModeChunked
	}
	return name
}
