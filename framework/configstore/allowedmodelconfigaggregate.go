package configstore

import (
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// scopeKey is the grouping key for aggregation.
type scopeKey struct {
	virtualKeyID string
	scopeOrgID   string
	providerID   string
}

func makeScopeKey(row tables.TableAllowedModelConfig) scopeKey {
	vkID := ""
	if row.VirtualKeyID != nil {
		vkID = *row.VirtualKeyID
	}
	orgID := ""
	if row.ScopeOrgID != nil {
		orgID = *row.ScopeOrgID
	}
	return scopeKey{virtualKeyID: vkID, scopeOrgID: orgID, providerID: row.ProviderID}
}

// AggregateAllowedModelConfigs groups raw DB rows by (VirtualKeyID|ScopeOrgID, ProviderID)
// and returns one synthetic TableAllowedModelConfig per group with:
//
//   - AllowedModels populated from all non-nil AllowedModel names in the group.
//   - BlacklistedModels populated from all non-nil BlacklistedModel names in the group.
//   - AllowedModels set to ["*"] when the group has rows but none carry an AllowedModelID
//     (allow-all-except-blacklist semantics).
//   - Weight / Keys / Budgets / RateLimits / AllowAllKeys taken from the "header" row —
//     the row where both AllowedModelID and BlacklistedModelID are nil.
//
// The function preserves insertion order for deterministic output.
func AggregateAllowedModelConfigs(rows []tables.TableAllowedModelConfig) []tables.TableAllowedModelConfig {
	if len(rows) == 0 {
		return nil
	}

	type group struct {
		header      *tables.TableAllowedModelConfig
		allowNames  []string
		blockNames  []string
		hasAllowRef bool // at least one row with a non-nil AllowedModelID
	}

	order := make([]scopeKey, 0, len(rows))
	groups := make(map[scopeKey]*group, len(rows))

	for i := range rows {
		row := &rows[i]
		key := makeScopeKey(*row)

		g, exists := groups[key]
		if !exists {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}

		isHeaderRow := row.AllowedModelID == nil && row.BlacklistedModelID == nil
		if isHeaderRow && g.header == nil {
			g.header = row
		}

		if row.AllowedModelID != nil {
			g.hasAllowRef = true
			if row.AllowedModel != nil && row.AllowedModel.Name != "" {
				g.allowNames = append(g.allowNames, row.AllowedModel.Name)
			} else if len(row.AllowedModels) > 0 {
				// Already hydrated (e.g. from a prior AfterFind)
				g.allowNames = append(g.allowNames, row.AllowedModels...)
			}
		}

		if row.BlacklistedModelID != nil {
			if row.BlacklistedModel != nil && row.BlacklistedModel.Name != "" {
				g.blockNames = append(g.blockNames, row.BlacklistedModel.Name)
			} else if len(row.BlacklistedModels) > 0 {
				g.blockNames = append(g.blockNames, row.BlacklistedModels...)
			}
		}
	}

	result := make([]tables.TableAllowedModelConfig, 0, len(order))
	for _, key := range order {
		g := groups[key]

		// Start from the header row if one exists, otherwise synthesise a minimal record.
		var synthetic tables.TableAllowedModelConfig
		if g.header != nil {
			synthetic = *g.header
		} else {
			// No header row — borrow identity fields from the first row in the group.
			// Locate first row for this key.
			for i := range rows {
				if makeScopeKey(rows[i]) == key {
					r := rows[i]
					synthetic.ID = r.ID
					synthetic.VirtualKeyID = r.VirtualKeyID
					synthetic.ScopeOrgID = r.ScopeOrgID
					synthetic.ProviderID = r.ProviderID
					synthetic.Provider = r.Provider
					synthetic.ConfigProvider = r.ConfigProvider
					synthetic.AllowAllKeys = r.AllowAllKeys
					synthetic.SystemColumns = r.SystemColumns
					break
				}
			}
		}

		// Clear the persisted single-model FK fields on the synthetic record.
		synthetic.AllowedModelID = nil
		synthetic.AllowedModel = nil
		synthetic.BlacklistedModelID = nil
		synthetic.BlacklistedModel = nil

		// Populate virtual lists.
		if g.hasAllowRef {
			synthetic.AllowedModels = schemas.WhiteList(g.allowNames)
		} else {
			// Rows exist for this scope+provider but none pin an allowed model →
			// all models are allowed (except those blacklisted).
			synthetic.AllowedModels = schemas.WhiteList{"*"}
		}

		if len(g.blockNames) > 0 {
			synthetic.BlacklistedModels = schemas.BlackList(g.blockNames)
		} else {
			synthetic.BlacklistedModels = nil
		}

		result = append(result, synthetic)
	}

	return result
}
