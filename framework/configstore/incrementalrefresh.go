package configstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// RefreshOverlap is subtracted from the last refresh watermark so rows updated
// exactly at the boundary or under minor clock skew are not missed.
const RefreshOverlap = 2 * time.Second

// GovernanceRefreshDelta holds governance entities changed since the previous refresh.
type GovernanceRefreshDelta struct {
	Organizations []tables.TableOrganization
	VirtualKeys   []tables.TableVirtualKey
	Budgets       []tables.TableBudget
	RateLimits    []tables.TableRateLimit
	ModelConfigs  []tables.TableModelConfig
	Providers     []tables.TableProvider
	RoutingRules  []tables.TableRoutingRule
}

// IsEmpty reports whether the delta contains no changes.
func (d *GovernanceRefreshDelta) IsEmpty() bool {
	if d == nil {
		return true
	}
	return len(d.Organizations) == 0 &&
		len(d.VirtualKeys) == 0 &&
		len(d.Budgets) == 0 &&
		len(d.RateLimits) == 0 &&
		len(d.ModelConfigs) == 0 &&
		len(d.Providers) == 0 &&
		len(d.RoutingRules) == 0
}

// ProviderConfigRefreshDelta holds provider runtime config changes since the last refresh.
type ProviderConfigRefreshDelta struct {
	Changed map[schemas.ModelProvider]ProviderConfig
	Removed []schemas.ModelProvider
}

// IsEmpty reports whether no provider config rows changed.
func (d *ProviderConfigRefreshDelta) IsEmpty() bool {
	if d == nil {
		return true
	}
	return len(d.Changed) == 0 && len(d.Removed) == 0
}

// GetGovernanceRefreshDelta loads governance entities with updated_at >= since.
func (s *RDBConfigStore) GetGovernanceRefreshDelta(ctx context.Context, since time.Time) (*GovernanceRefreshDelta, error) {
	if since.IsZero() {
		return nil, fmt.Errorf("governance refresh since watermark is required")
	}
	since = NormalizeRefreshSince(since)
	db := s.DB().WithContext(ctx)
	delta := &GovernanceRefreshDelta{}

	if err := db.Where("updated_at >= ?", since).Find(&delta.Organizations).Error; err != nil {
		return nil, fmt.Errorf("organizations changed since: %w", err)
	}
	if err := appendDeletedRowsSince(db, since, &delta.Organizations); err != nil {
		return nil, err
	}

	if err := GovernanceActive(db.Where("updated_at >= ?", since)).Find(&delta.Budgets).Error; err != nil {
		return nil, fmt.Errorf("budgets changed since: %w", err)
	}
	if err := appendDeletedRowsSince(db, since, &delta.Budgets); err != nil {
		return nil, err
	}

	if err := GovernanceActive(db.Where("updated_at >= ?", since)).Find(&delta.RateLimits).Error; err != nil {
		return nil, fmt.Errorf("rate limits changed since: %w", err)
	}
	if err := appendDeletedRowsSince(db, since, &delta.RateLimits); err != nil {
		return nil, err
	}

	pre := governanceActivePreload()
	if err := GovernanceActive(db.Where("updated_at >= ?", since)).
		Preload("Budgets", pre).
		Preload("RateLimits", pre).
		Find(&delta.ModelConfigs).Error; err != nil {
		return nil, fmt.Errorf("model configs changed since: %w", err)
	}
	if err := appendDeletedRowsSince(db, since, &delta.ModelConfigs); err != nil {
		return nil, err
	}

	if err := ActiveRows(db.Where("updated_at >= ?", since)).
		Preload("Budgets").
		Preload("RateLimits").
		Find(&delta.Providers).Error; err != nil {
		return nil, fmt.Errorf("providers changed since: %w", err)
	}
	if err := appendDeletedRowsSince(db, since, &delta.Providers); err != nil {
		return nil, err
	}
	if err := s.loadRoutingRulesOrdered(ctx, &delta.RoutingRules, func(q *gorm.DB) *gorm.DB {
		return q.Where("updated_at >= ?", since)
	}); err != nil {
		return nil, fmt.Errorf("routing rules changed since: %w", err)
	}

	vkIDs, err := s.collectVirtualKeyIDsChangedSince(ctx, since)
	if err != nil {
		return nil, err
	}
	for _, vkID := range vkIDs {
		vk, loadErr := s.GetVirtualKey(ctx, vkID)
		if loadErr != nil {
			if IsNotFound(loadErr) {
				continue
			}
			return nil, fmt.Errorf("reload virtual key %s: %w", vkID, loadErr)
		}
		delta.VirtualKeys = append(delta.VirtualKeys, *vk)
	}

	if err := appendDeletedRowsSince(db, since, &delta.VirtualKeys); err != nil {
		return nil, err
	}

	return delta, nil
}

func appendDeletedRowsSince[T any](db *gorm.DB, since time.Time, dest *[]T) error {
	var deleted []T
	if err := db.Unscoped().Where("updated_at >= ? AND deleted = ?", since, true).Find(&deleted).Error; err != nil {
		return fmt.Errorf("deleted rows since: %w", err)
	}
	*dest = append(*dest, deleted...)
	return nil
}

func (s *RDBConfigStore) collectVirtualKeyIDsChangedSince(ctx context.Context, since time.Time) ([]string, error) {
	type idRow struct {
		ID string
	}
	var rows []idRow
	db := s.DB().WithContext(ctx)
	err := db.Raw(`
		SELECT DISTINCT id::text FROM (
			SELECT id FROM governance_virtual_keys WHERE updated_at >= ? AND deleted = false
			UNION
			SELECT virtual_key_id AS id FROM governance_virtual_key_provider_configs
				WHERE updated_at >= ? AND virtual_key_id IS NOT NULL
			UNION
			SELECT virtual_key_id AS id FROM governance_virtual_key_mcp_configs
				WHERE updated_at >= ? AND virtual_key_id IS NOT NULL
			UNION
			SELECT virtual_key_id AS id FROM governance_budgets
				WHERE updated_at >= ? AND virtual_key_id IS NOT NULL
			UNION
			SELECT pc.virtual_key_id AS id FROM governance_virtual_key_provider_configs pc
				INNER JOIN governance_budgets b ON b.provider_config_id = pc.id
				WHERE b.updated_at >= ? AND pc.virtual_key_id IS NOT NULL
			UNION
			SELECT pc.virtual_key_id AS id FROM governance_virtual_key_provider_configs pc
				INNER JOIN governance_virtual_key_provider_config_keys j
					ON j.table_virtual_key_provider_config_id = pc.id
				WHERE j.updated_at >= ? AND pc.virtual_key_id IS NOT NULL
		) changed_vks
		WHERE id IS NOT NULL
	`, since, since, since, since, since, since).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("collect changed virtual key ids: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// GetProviderConfigRefreshDelta loads provider runtime configs for providers (or their keys)
// with updated_at >= since.
func (s *RDBConfigStore) GetProviderConfigRefreshDelta(ctx context.Context, since time.Time) (*ProviderConfigRefreshDelta, error) {
	if since.IsZero() {
		return nil, fmt.Errorf("provider config refresh since watermark is required")
	}
	since = NormalizeRefreshSince(since)
	db := s.DB().WithContext(ctx)
	delta := &ProviderConfigRefreshDelta{
		Changed: make(map[schemas.ModelProvider]ProviderConfig),
	}

	type nameRow struct {
		Name string
	}
	var activeNames []nameRow
	if err := db.Model(&tables.TableProvider{}).
		Where("updated_at >= ? AND deleted = ?", since, false).
		Select("name").
		Find(&activeNames).Error; err != nil {
		return nil, fmt.Errorf("changed providers: %w", err)
	}
	var keyProviderNames []nameRow
	if err := db.Table("config_keys").
		Select("DISTINCT config_providers.name AS name").
		Joins("JOIN config_providers ON config_providers.id = config_keys.provider_id").
		Where("config_keys.updated_at >= ? AND config_keys.deleted = ? AND config_providers.deleted = ?", since, false, false).
		Scan(&keyProviderNames).Error; err != nil {
		return nil, fmt.Errorf("changed provider keys: %w", err)
	}

	seen := make(map[string]struct{})
	for _, row := range append(activeNames, keyProviderNames...) {
		if row.Name == "" {
			continue
		}
		if _, ok := seen[row.Name]; ok {
			continue
		}
		seen[row.Name] = struct{}{}
		cfg, err := s.GetProviderConfig(ctx, schemas.ModelProvider(row.Name))
		if err != nil {
			if IsNotFound(err) {
				delta.Removed = append(delta.Removed, schemas.ModelProvider(row.Name))
				continue
			}
			return nil, fmt.Errorf("reload provider %s: %w", row.Name, err)
		}
		delta.Changed[schemas.ModelProvider(row.Name)] = *cfg
	}

	var removed []nameRow
	if err := db.Model(&tables.TableProvider{}).
		Where("updated_at >= ? AND deleted = ?", since, true).
		Select("name").
		Find(&removed).Error; err != nil {
		return nil, fmt.Errorf("removed providers: %w", err)
	}
	for _, row := range removed {
		if row.Name == "" {
			continue
		}
		delta.Removed = append(delta.Removed, schemas.ModelProvider(row.Name))
	}

	return delta, nil
}

// IsNotFound reports whether err is a config store not-found error.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
