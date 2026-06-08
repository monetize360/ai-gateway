package configstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// NotDeleted scopes queries to non-soft-deleted rows.
func NotDeleted(db *gorm.DB) *gorm.DB {
	return db.Where("deleted = ?", false)
}

// ActiveRows scopes queries to non-soft-deleted rows (governance and config tables).
func ActiveRows(db *gorm.DB) *gorm.DB {
	return NotDeleted(db)
}

func auditUserID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(schemas.BifrostContextKeyUserID).(string); ok {
		return v
	}
	return ""
}

// ApplyAuditOnCreate stamps create/update timestamps and audit user IDs on a
// governance row before insert.
func ApplyAuditOnCreate(ctx context.Context, cols *tables.SystemColumns) {
	if cols == nil {
		return
	}
	now := time.Now().UTC()
	if cols.CreatedAt.IsZero() {
		cols.CreatedAt = now
	}
	cols.UpdatedAt = now
	if userID := auditUserID(ctx); userID != "" {
		uid := userID
		cols.CreatedBy = &uid
		cols.UpdatedBy = &uid
	}
}

// ApplyAuditOnUpdate stamps update timestamp and updated_by before persist.
func ApplyAuditOnUpdate(ctx context.Context, cols *tables.SystemColumns) {
	if cols == nil {
		return
	}
	cols.UpdatedAt = time.Now().UTC()
	if userID := auditUserID(ctx); userID != "" {
		uid := userID
		cols.UpdatedBy = &uid
	}
}

// SoftDelete marks a governance row deleted and stamps audit metadata.
func SoftDelete(ctx context.Context, cols *tables.SystemColumns) {
	if cols == nil {
		return
	}
	cols.Deleted = true
	ApplyAuditOnUpdate(ctx, cols)
}

// GovernanceActive scopes queries to non-soft-deleted governance rows.
func GovernanceActive(db *gorm.DB) *gorm.DB {
	return ActiveRows(db)
}

// MarkDeleted performs a soft-delete update on matching rows.
func MarkDeleted(ctx context.Context, tx *gorm.DB, model any, query any, args ...any) error {
	return MarkGovernanceDeleted(ctx, tx, model, query, args...)
}

// EnsureGovernanceRowID assigns a UUID primary key when unset.
func EnsureGovernanceRowID(id *string) {
	if id == nil || *id != "" {
		return
	}
	next := uuid.NewString()
	*id = next
}

// MarkGovernanceDeleted performs a soft-delete update on matching rows.
func MarkGovernanceDeleted(ctx context.Context, tx *gorm.DB, model any, query any, args ...any) error {
	updates := map[string]any{
		"deleted":    true,
		"updated_at": time.Now().UTC(),
	}
	if userID := auditUserID(ctx); userID != "" {
		updates["updated_by"] = userID
	}
	return tx.Session(&gorm.Session{SkipHooks: true}).
		Model(model).
		Where(query, args...).
		Updates(updates).Error
}
