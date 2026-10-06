package tables

import "time"

// TableAlertNotification is the dedupe and audit row for one claimed threshold crossing.
// published stays false until Kafka accepts the event. notified is set by MPilot
// after the email for this notification id is sent.
type TableAlertNotification struct {
	ID            string    `gorm:"primaryKey;type:uuid" json:"id"`
	ThresholdID   string    `gorm:"column:threshold_id;type:uuid;index" json:"threshold_id"`
	PeriodKey     string    `gorm:"column:period_key;type:varchar(128)" json:"period_key"`
	ObservedValue float64   `gorm:"column:observed_value" json:"observed_value"`
	FiredAt       time.Time `gorm:"column:fired_at" json:"fired_at"`
	Published     bool      `gorm:"column:published;not null;default:false" json:"published"`
	Notified      bool      `gorm:"column:notified;not null;default:false" json:"notified"`

	SystemColumns
}

// TableName sets the table name for AlertNotification.
func (TableAlertNotification) TableName() string { return "alertnotification__m" }
