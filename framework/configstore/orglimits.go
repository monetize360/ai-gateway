package configstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// GetOrganizations retrieves all organizations from the tenant database (read-only for governance hierarchy).
func (s *RDBConfigStore) GetOrganizations(ctx context.Context) ([]tables.TableOrganization, error) {
	var organizations []tables.TableOrganization
	if err := GovernanceActive(s.ScopedDB(ctx)).
		Order("created_at ASC").
		Find(&organizations).Error; err != nil {
		return nil, err
	}
	return organizations, nil
}

func preloadOrgLimitRelations(db *gorm.DB) *gorm.DB {
	pre := governanceActivePreload()
	return GovernanceActive(db).
		Preload("Budget", pre).
		Preload("RateLimit", pre)
}

// GetOrgLimits retrieves all org limit rows with budget and rate-limit relationships.
// At most one active row exists per org_id (enforced by idx_governance_org_limits_org_id).
func (s *RDBConfigStore) GetOrgLimits(ctx context.Context) ([]tables.TableOrgLimit, error) {
	var orgLimits []tables.TableOrgLimit
	if err := preloadOrgLimitRelations(GovernanceActive(s.ScopedDB(ctx))).
		Order("created_at ASC").
		Find(&orgLimits).Error; err != nil {
		return nil, err
	}
	return orgLimits, nil
}

// GetOrgLimit retrieves an org limit row by primary key.
func (s *RDBConfigStore) GetOrgLimit(ctx context.Context, id string) (*tables.TableOrgLimit, error) {
	var orgLimit tables.TableOrgLimit
	if err := preloadOrgLimitRelations(GovernanceActive(s.ScopedDB(ctx))).
		Where("id = ?", id).
		First(&orgLimit).Error; err != nil {
		return nil, err
	}
	return &orgLimit, nil
}

// GetOrgLimitByOrgID retrieves the single org limit row for a given organization.
func (s *RDBConfigStore) GetOrgLimitByOrgID(ctx context.Context, orgID string) (*tables.TableOrgLimit, error) {
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return nil, fmt.Errorf("org_id is required")
	}
	var orgLimit tables.TableOrgLimit
	if err := preloadOrgLimitRelations(GovernanceActive(s.ScopedDB(ctx))).
		Where("org_id = ?", orgID).
		First(&orgLimit).Error; err != nil {
		return nil, err
	}
	return &orgLimit, nil
}

func (s *RDBConfigStore) orgLimitExistsForOrg(ctx context.Context, db *gorm.DB, orgID string, excludeID string) (bool, error) {
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return false, fmt.Errorf("org_id is required")
	}
	query := GovernanceActive(db.WithContext(ctx).Model(&tables.TableOrgLimit{})).Where("org_id = ?", orgID)
	if excludeID != "" {
		query = query.Where("id <> ?", excludeID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateOrgLimit creates a new org limit row. Only one active row is allowed per org_id.
func (s *RDBConfigStore) CreateOrgLimit(ctx context.Context, orgLimit *tables.TableOrgLimit, tx ...*gorm.DB) error {
	if orgLimit == nil {
		return fmt.Errorf("org limit is required")
	}
	db := s.ScopedDB(ctx)
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	exists, err := s.orgLimitExistsForOrg(ctx, db, orgLimit.OrgID, "")
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("governance org limit already exists for org %s: %w", orgLimit.OrgID, ErrAlreadyExists)
	}
	if err := db.WithContext(ctx).Create(orgLimit).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// UpdateOrgLimit updates an existing org limit row, preserving the one-row-per-org invariant.
func (s *RDBConfigStore) UpdateOrgLimit(ctx context.Context, orgLimit *tables.TableOrgLimit, tx ...*gorm.DB) error {
	if orgLimit == nil {
		return fmt.Errorf("org limit is required")
	}
	db := s.ScopedDB(ctx)
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	exists, err := s.orgLimitExistsForOrg(ctx, db, orgLimit.OrgID, orgLimit.ID)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("governance org limit already exists for org %s: %w", orgLimit.OrgID, ErrAlreadyExists)
	}
	if err := db.WithContext(ctx).Save(orgLimit).Error; err != nil {
		return s.parseGormError(err)
	}
	return nil
}

// DeleteOrgLimit soft-deletes an org limit row by primary key.
func (s *RDBConfigStore) DeleteOrgLimit(ctx context.Context, id string, tx ...*gorm.DB) error {
	db := s.ScopedDB(ctx)
	if len(tx) > 0 && tx[0] != nil {
		db = tx[0]
	}
	result := db.WithContext(ctx).Where("id = ?", id).Delete(&tables.TableOrgLimit{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
