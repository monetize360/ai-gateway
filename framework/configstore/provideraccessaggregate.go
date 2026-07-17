package configstore

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// ProviderAccessPolicy is an alias for the runtime type in tables package.
type ProviderAccessPolicy = tables.ProviderAccessPolicyRT

// AggregateProviderAccess converts raw TableProviderAccess rows (all sharing
// the same VK or org scope) into a single ProviderAccessPolicy.
//
//   - Wildcard allowed row   → AllowedProviders = ["*"]
//   - Wildcard blocked row   → BlacklistedProviders = ["*"]
//   - Non-wildcard rows      → provider name appended to the corresponding list
func AggregateProviderAccess(rows []tables.TableProviderAccess) *ProviderAccessPolicy {
	if len(rows) == 0 {
		return nil
	}
	policy := &ProviderAccessPolicy{}
	for i := range rows {
		row := &rows[i]
		if row.IsWildcard {
			switch {
			case tables.IsAllowedAccessType(row.AccessType):
				policy.AllowedProviders = schemas.WhiteList{"*"}
			case tables.IsBlockedAccessType(row.AccessType):
				policy.BlacklistedProviders = schemas.BlackList{"*"}
			}
			continue
		}
		name := row.ProviderName()
		if name == "" {
			continue
		}
		switch {
		case tables.IsAllowedAccessType(row.AccessType):
			policy.AllowedProviders = append(policy.AllowedProviders, name)
		case tables.IsBlockedAccessType(row.AccessType):
			policy.BlacklistedProviders = append(policy.BlacklistedProviders, name)
		}
	}
	return policy
}

// MergeProviderAccessPolicies merges multiple policies (e.g. from ancestor orgs)
// into a single effective policy using most-restrictive-wins semantics:
//
//   - Blacklists: union (any ancestor block = blocked)
//   - Allowlists: intersect when both are restrictive; if either is unrestricted
//     (["*"] or empty), use the other's restrictive set
func MergeProviderAccessPolicies(policies ...*ProviderAccessPolicy) *ProviderAccessPolicy {
	// Filter out nil/empty policies
	var nonEmpty []*ProviderAccessPolicy
	for _, p := range policies {
		if !p.IsEmpty() {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}
	if len(nonEmpty) == 1 {
		return nonEmpty[0]
	}

	merged := &ProviderAccessPolicy{}

	// Union blacklists
	blockAll := false
	blockSeen := make(map[string]bool)
	for _, p := range nonEmpty {
		if p.BlacklistedProviders.IsBlockAll() {
			blockAll = true
			break
		}
		for _, name := range p.BlacklistedProviders {
			lower := strings.ToLower(name)
			if !blockSeen[lower] {
				blockSeen[lower] = true
				merged.BlacklistedProviders = append(merged.BlacklistedProviders, name)
			}
		}
	}
	if blockAll {
		merged.BlacklistedProviders = schemas.BlackList{"*"}
	}

	// Intersect allowlists: collect all restrictive sets, then intersect
	var restrictiveSets []schemas.WhiteList
	for _, p := range nonEmpty {
		if len(p.AllowedProviders) == 0 || p.AllowedProviders.IsUnrestricted() {
			continue
		}
		restrictiveSets = append(restrictiveSets, p.AllowedProviders)
	}

	switch len(restrictiveSets) {
	case 0:
		// No restrictive allowlists — no allow restriction
	case 1:
		merged.AllowedProviders = restrictiveSets[0]
	default:
		merged.AllowedProviders = intersectWhiteLists(restrictiveSets)
	}

	return merged
}

func intersectWhiteLists(sets []schemas.WhiteList) schemas.WhiteList {
	if len(sets) == 0 {
		return nil
	}
	// Start with the first set as candidates
	candidates := make(map[string]string, len(sets[0]))
	for _, v := range sets[0] {
		candidates[strings.ToLower(v)] = v
	}
	// Intersect with each subsequent set
	for _, set := range sets[1:] {
		present := make(map[string]bool, len(set))
		for _, v := range set {
			present[strings.ToLower(v)] = true
		}
		for key := range candidates {
			if !present[key] {
				delete(candidates, key)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	result := make(schemas.WhiteList, 0, len(candidates))
	// Preserve original-case from the first set
	for _, v := range sets[0] {
		if _, ok := candidates[strings.ToLower(v)]; ok {
			result = append(result, v)
		}
	}
	return result
}

