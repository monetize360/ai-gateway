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

func (gs *LocalGovernanceStore) loadOrgLimit(orgID string) *configstoreTables.TableOrgLimit {
	limitValue, exists := gs.orgLimits.Load(orgID)
	if !exists || limitValue == nil {
		return nil
	}
	limit, ok := limitValue.(*configstoreTables.TableOrgLimit)
	if !ok {
		return nil
	}
	return limit
}

func (gs *LocalGovernanceStore) appendOrgHierarchyBudgets(orgID string, entityWiseBudgets EntityWiseBudgets, seen map[string]bool) {
	gs.walkOrgAncestors(orgID, func(currentOrgID string) bool {
		limit := gs.loadOrgLimit(currentOrgID)
		if limit == nil || limit.BudgetID == nil {
			return true
		}
		if seen[*limit.BudgetID] {
			return true
		}
		if budgetValue, exists := gs.budgets.Load(*limit.BudgetID); exists && budgetValue != nil {
			if budget, ok := budgetValue.(*configstoreTables.TableBudget); ok && budget != nil {
				key := "Org:" + currentOrgID
				if categoryBudgets := entityWiseBudgets[key]; categoryBudgets == nil {
					entityWiseBudgets[key] = []*configstoreTables.TableBudget{}
				}
				entityWiseBudgets[key] = append(entityWiseBudgets[key], budget)
				seen[budget.ID] = true
			}
		}
		return true
	})
}

func (gs *LocalGovernanceStore) appendOrgHierarchyRateLimits(orgID string, rateLimitsWithCategories map[string][]*configstoreTables.TableRateLimit, seen map[string]bool) {
	gs.walkOrgAncestors(orgID, func(currentOrgID string) bool {
		limit := gs.loadOrgLimit(currentOrgID)
		if limit == nil || limit.RateLimitID == nil {
			return true
		}
		if seen[*limit.RateLimitID] {
			return true
		}
		if rateLimitValue, exists := gs.rateLimits.Load(*limit.RateLimitID); exists && rateLimitValue != nil {
			if rateLimit, ok := rateLimitValue.(*configstoreTables.TableRateLimit); ok && rateLimit != nil {
				key := "Org:" + currentOrgID
				if categoryRateLimits := rateLimitsWithCategories[key]; categoryRateLimits == nil {
					rateLimitsWithCategories[key] = []*configstoreTables.TableRateLimit{}
				}
				rateLimitsWithCategories[key] = append(rateLimitsWithCategories[key], rateLimit)
				seen[rateLimit.ID] = true
			}
		}
		return true
	})
}
