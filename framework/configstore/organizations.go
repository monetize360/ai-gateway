package configstore

import (
	"context"

	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// GetOrganizations loads organization rows for org-hierarchy governance walks.
func (s *RDBConfigStore) GetOrganizations(ctx context.Context) ([]tables.TableOrganization, error) {
	var organizations []tables.TableOrganization
	if err := s.ScopedDB(ctx).Where("deleted = ?", false).Find(&organizations).Error; err != nil {
		return nil, err
	}
	return organizations, nil
}
