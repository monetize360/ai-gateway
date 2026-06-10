package governance

import (
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// walkOrgAncestors visits orgID and each ancestor up to the root (child → parent).
// The visitor returns false to stop walking; true continues to the parent.
func (gs *LocalGovernanceStore) walkOrgAncestors(orgID string, visit func(orgID string) bool) {
	visited := map[string]bool{}
	for orgID != "" {
		if visited[orgID] {
			break
		}
		visited[orgID] = true
		if !visit(orgID) {
			return
		}
		orgValue, exists := gs.organizations.Load(orgID)
		if !exists || orgValue == nil {
			break
		}
		org, ok := orgValue.(*configstoreTables.TableOrganization)
		if !ok || org == nil || org.ParentOrgID == nil {
			break
		}
		orgID = *org.ParentOrgID
	}
}

func (gs *LocalGovernanceStore) appendOrgHierarchyBudgets(orgID string, entityWiseBudgets EntityWiseBudgets, seen map[string]bool) {
	gs.walkOrgAncestors(orgID, func(currentOrgID string) bool {
		gs.budgets.Range(func(_, value interface{}) bool {
			budget, ok := value.(*configstoreTables.TableBudget)
			if !ok || budget == nil || budget.GovernedOrganizationID == nil || *budget.GovernedOrganizationID != currentOrgID {
				return true
			}
			if seen[budget.ID] {
				return true
			}
			key := "Org:" + currentOrgID
			entityWiseBudgets[key] = append(entityWiseBudgets[key], budget)
			seen[budget.ID] = true
			return true
		})
		return true
	})
}

func (gs *LocalGovernanceStore) appendOrgHierarchyRateLimits(orgID string, rateLimitsWithCategories map[string][]*configstoreTables.TableRateLimit, seen map[string]bool) {
	gs.walkOrgAncestors(orgID, func(currentOrgID string) bool {
		gs.rateLimits.Range(func(_, value interface{}) bool {
			rateLimit, ok := value.(*configstoreTables.TableRateLimit)
			if !ok || rateLimit == nil || rateLimit.GovernedOrganizationID == nil || *rateLimit.GovernedOrganizationID != currentOrgID {
				return true
			}
			if seen[rateLimit.ID] {
				return true
			}
			key := "Org:" + currentOrgID
			rateLimitsWithCategories[key] = append(rateLimitsWithCategories[key], rateLimit)
			seen[rateLimit.ID] = true
			return true
		})
		return true
	})
}
