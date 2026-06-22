package governance

import (
	"context"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

func attachGovernanceFromReverseFK(
	budgets []configstoreTables.TableBudget,
	rateLimits []configstoreTables.TableRateLimit,
	providers []configstoreTables.TableProvider,
	modelConfigs []configstoreTables.TableModelConfig,
	virtualKeys []configstoreTables.TableVirtualKey,
) {
	configstore.AttachGovernanceFromReverseFK(budgets, rateLimits, providers, modelConfigs, virtualKeys)
}

func budgetID(b *configstoreTables.TableBudget) string {
	return configstoreTables.BudgetRefID(b)
}

func rateLimitID(rl *configstoreTables.TableRateLimit) string {
	return configstoreTables.RateLimitRefID(rl)
}

func collectBudgetIDsFromGovernanceRefreshDelta(delta *configstore.GovernanceRefreshDelta) map[string]struct{} {
	ids := make(map[string]struct{})
	if delta == nil {
		return ids
	}
	for i := range delta.Budgets {
		if id := delta.Budgets[i].ID; id != "" && !delta.Budgets[i].Deleted {
			ids[id] = struct{}{}
		}
	}
	for i := range delta.VirtualKeys {
		vk := &delta.VirtualKeys[i]
		if vk.Deleted {
			continue
		}
		for j := range vk.Budgets {
			if id := vk.Budgets[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
		for j := range vk.ProviderConfigs {
			for k := range vk.ProviderConfigs[j].Budgets {
				if id := vk.ProviderConfigs[j].Budgets[k].ID; id != "" {
					ids[id] = struct{}{}
				}
			}
		}
	}
	for i := range delta.Providers {
		if delta.Providers[i].Deleted {
			continue
		}
		for j := range delta.Providers[i].Budgets {
			if id := delta.Providers[i].Budgets[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	for i := range delta.ModelConfigs {
		if delta.ModelConfigs[i].Deleted {
			continue
		}
		for j := range delta.ModelConfigs[i].Budgets {
			if id := delta.ModelConfigs[i].Budgets[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func collectRateLimitIDsFromGovernanceRefreshDelta(delta *configstore.GovernanceRefreshDelta) map[string]struct{} {
	ids := make(map[string]struct{})
	if delta == nil {
		return ids
	}
	for i := range delta.RateLimits {
		if id := delta.RateLimits[i].ID; id != "" && !delta.RateLimits[i].Deleted {
			ids[id] = struct{}{}
		}
	}
	for i := range delta.VirtualKeys {
		vk := &delta.VirtualKeys[i]
		if vk.Deleted {
			continue
		}
		for j := range vk.RateLimits {
			if id := vk.RateLimits[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
		for j := range vk.ProviderConfigs {
			for k := range vk.ProviderConfigs[j].RateLimits {
				if id := vk.ProviderConfigs[j].RateLimits[k].ID; id != "" {
					ids[id] = struct{}{}
				}
			}
		}
	}
	for i := range delta.Providers {
		if delta.Providers[i].Deleted {
			continue
		}
		for j := range delta.Providers[i].RateLimits {
			if id := delta.Providers[i].RateLimits[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	for i := range delta.ModelConfigs {
		if delta.ModelConfigs[i].Deleted {
			continue
		}
		for j := range delta.ModelConfigs[i].RateLimits {
			if id := delta.ModelConfigs[i].RateLimits[j].ID; id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

// hydrateEmbeddedBudgetForParent returns the budget snapshot to attach on a parent
// entity (VK, provider, model config). Existing budgets are read from the canonical
// budgets map so association reloads cannot clobber config fields such as soft_limit.
func hydrateEmbeddedBudgetForParent(gs *LocalGovernanceStore, ctx context.Context, incoming configstoreTables.TableBudget, calendarAligned bool) configstoreTables.TableBudget {
	if incoming.ID == "" {
		return incoming
	}
	if canonical := gs.LoadBudget(ctx, incoming.ID); canonical != nil {
		out := *canonical
		out.IsCalendarAligned = calendarAligned
		return out
	}
	incoming.IsCalendarAligned = calendarAligned
	gs.UpsertBudgetConfig(ctx, incoming.ID, &incoming)
	if canonical := gs.LoadBudget(ctx, incoming.ID); canonical != nil {
		out := *canonical
		out.IsCalendarAligned = calendarAligned
		return out
	}
	return incoming
}

// hydrateEmbeddedRateLimitForParent mirrors hydrateEmbeddedBudgetForParent for rate limits.
func hydrateEmbeddedRateLimitForParent(gs *LocalGovernanceStore, ctx context.Context, incoming configstoreTables.TableRateLimit, calendarAligned bool) configstoreTables.TableRateLimit {
	if incoming.ID == "" {
		return incoming
	}
	if canonical := gs.LoadRateLimit(ctx, incoming.ID); canonical != nil {
		out := *canonical
		out.IsCalendarAligned = calendarAligned
		return out
	}
	incoming.IsCalendarAligned = calendarAligned
	gs.UpsertRateLimitConfig(ctx, incoming.ID, &incoming)
	if canonical := gs.LoadRateLimit(ctx, incoming.ID); canonical != nil {
		out := *canonical
		out.IsCalendarAligned = calendarAligned
		return out
	}
	return incoming
}

func loadLiveBudgets(gs *LocalGovernanceStore, ctx context.Context, budgets []configstoreTables.TableBudget) []*configstoreTables.TableBudget {
	if len(budgets) == 0 {
		return nil
	}
	result := make([]*configstoreTables.TableBudget, 0, len(budgets))
	for i := range budgets {
		if b := gs.LoadBudget(ctx, budgets[i].ID); b != nil {
			result = append(result, b)
		}
	}
	return result
}

func loadLiveRateLimits(gs *LocalGovernanceStore, ctx context.Context, rateLimits []configstoreTables.TableRateLimit) []*configstoreTables.TableRateLimit {
	if len(rateLimits) == 0 {
		return nil
	}
	result := make([]*configstoreTables.TableRateLimit, 0, len(rateLimits))
	for i := range rateLimits {
		if rl := gs.LoadRateLimit(ctx, rateLimits[i].ID); rl != nil {
			result = append(result, rl)
		}
	}
	return result
}

func appendBudgetIDs(ids []string, seen map[string]bool, budgets []configstoreTables.TableBudget) []string {
	for i := range budgets {
		id := budgets[i].ID
		if id == "" || seen[id] {
			continue
		}
		ids = append(ids, id)
		seen[id] = true
	}
	return ids
}

func appendRateLimitIDs(ids []string, seen map[string]bool, rateLimits []configstoreTables.TableRateLimit) []string {
	for i := range rateLimits {
		id := rateLimits[i].ID
		if id == "" || seen[id] {
			continue
		}
		ids = append(ids, id)
		seen[id] = true
	}
	return ids
}

func bumpBudgetSlice(ctx context.Context, gs *LocalGovernanceStore, budgets []configstoreTables.TableBudget, cost float64) error {
	for i := range budgets {
		if err := gs.BumpBudgetUsage(ctx, budgets[i].ID, cost); err != nil {
			return err
		}
	}
	return nil
}

func bumpRateLimitSlice(ctx context.Context, gs *LocalGovernanceStore, rateLimits []configstoreTables.TableRateLimit, tokensUsed int64, shouldUpdateTokens, shouldUpdateRequests bool) error {
	for i := range rateLimits {
		if err := gs.BumpRateLimitUsage(ctx, rateLimits[i].ID, tokensUsed, shouldUpdateTokens, shouldUpdateRequests); err != nil {
			return err
		}
	}
	return nil
}

func hydrateBudgetSlice(budgets []configstoreTables.TableBudget, gs *LocalGovernanceStore) []configstoreTables.TableBudget {
	if len(budgets) == 0 {
		return budgets
	}
	out := make([]configstoreTables.TableBudget, len(budgets))
	copy(out, budgets)
	for i := range out {
		if liveBudget, exists := gs.budgets.Load(out[i].ID); exists && liveBudget != nil {
			if b, ok := liveBudget.(*configstoreTables.TableBudget); ok && b != nil {
				out[i] = *b
			}
		}
	}
	return out
}

func hydrateRateLimitSlice(rateLimits []configstoreTables.TableRateLimit, gs *LocalGovernanceStore) []configstoreTables.TableRateLimit {
	if len(rateLimits) == 0 {
		return rateLimits
	}
	out := make([]configstoreTables.TableRateLimit, len(rateLimits))
	copy(out, rateLimits)
	for i := range out {
		if liveRL, exists := gs.rateLimits.Load(out[i].ID); exists && liveRL != nil {
			if rl, ok := liveRL.(*configstoreTables.TableRateLimit); ok && rl != nil {
				out[i] = *rl
			}
		}
	}
	return out
}

func hydrateModelConfigGovernance(clone *configstoreTables.TableModelConfig, gs *LocalGovernanceStore) {
	if clone == nil {
		return
	}
	clone.Budgets = hydrateBudgetSlice(clone.Budgets, gs)
	clone.RateLimits = hydrateRateLimitSlice(clone.RateLimits, gs)
}

func hydrateProviderGovernance(clone *configstoreTables.TableProvider, gs *LocalGovernanceStore) {
	if clone == nil {
		return
	}
	clone.Budgets = hydrateBudgetSlice(clone.Budgets, gs)
	clone.RateLimits = hydrateRateLimitSlice(clone.RateLimits, gs)
}

func hydrateVirtualKeyRateLimits(vk *configstoreTables.TableVirtualKey, gs *LocalGovernanceStore) {
	if vk == nil {
		return
	}
	vk.RateLimits = hydrateRateLimitSlice(vk.RateLimits, gs)
	for i := range vk.ProviderConfigs {
		vk.ProviderConfigs[i].RateLimits = hydrateRateLimitSlice(vk.ProviderConfigs[i].RateLimits, gs)
	}
}

func appendLiveRateLimitsFromSlice(
	gs *LocalGovernanceStore,
	target map[string][]*configstoreTables.TableRateLimit,
	category string,
	rateLimits []configstoreTables.TableRateLimit,
	seen map[string]bool,
) {
	for i := range rateLimits {
		id := rateLimits[i].ID
		if id == "" || seen[id] {
			continue
		}
		if rateLimitValue, exists := gs.rateLimits.Load(id); exists && rateLimitValue != nil {
			if rateLimit, ok := rateLimitValue.(*configstoreTables.TableRateLimit); ok && rateLimit != nil {
				target[category] = append(target[category], rateLimit)
				seen[id] = true
			}
		}
	}
}

func applyRateLimitStatusFromSlice(gs *LocalGovernanceStore, rateLimits []configstoreTables.TableRateLimit, tokenBaselines map[string]int64, requestBaselines map[string]int64, result *BudgetAndRateLimitStatus) {
	for i := range rateLimits {
		if rateLimitValue, ok := gs.rateLimits.Load(rateLimits[i].ID); ok && rateLimitValue != nil {
			if rateLimit, ok := rateLimitValue.(*configstoreTables.TableRateLimit); ok && rateLimit != nil {
				tokensBaseline := tokenBaselines[rateLimit.ID]
				requestsBaseline := requestBaselines[rateLimit.ID]
				if rateLimit.TokenMaxLimit != nil && *rateLimit.TokenMaxLimit > 0 {
					tokenPercent := float64(rateLimit.TokenCurrentUsage+tokensBaseline) / float64(*rateLimit.TokenMaxLimit) * 100
					if tokenPercent > result.RateLimitTokenPercentUsed {
						result.RateLimitTokenPercentUsed = tokenPercent
					}
					if tokenPercent >= 100.0 && rateLimit.SoftLimit {
						result.SoftLimitExceeded = true
					}
				}
				if rateLimit.RequestMaxLimit != nil && *rateLimit.RequestMaxLimit > 0 {
					requestPercent := float64(rateLimit.RequestCurrentUsage+requestsBaseline) / float64(*rateLimit.RequestMaxLimit) * 100
					if requestPercent > result.RateLimitRequestPercentUsed {
						result.RateLimitRequestPercentUsed = requestPercent
					}
					if requestPercent >= 100.0 && rateLimit.SoftLimit {
						result.SoftLimitExceeded = true
					}
				}
			}
		}
	}
}

func applyBudgetStatusFromSlice(gs *LocalGovernanceStore, budgets []configstoreTables.TableBudget, budgetBaselines map[string]float64, result *BudgetAndRateLimitStatus) {
	for i := range budgets {
		if budgetValue, ok := gs.budgets.Load(budgets[i].ID); ok && budgetValue != nil {
			if budget, ok := budgetValue.(*configstoreTables.TableBudget); ok && budget != nil {
				baseline := budgetBaselines[budget.ID]
				if budget.MaxLimit > 0 {
					budgetPercent := float64(budget.CurrentUsage+baseline) / budget.MaxLimit * 100
					if budgetPercent > result.BudgetPercentUsed {
						result.BudgetPercentUsed = budgetPercent
					}
					if budgetPercent >= 100.0 && budget.SoftLimit {
						result.SoftLimitExceeded = true
					}
				}
			}
		}
	}
}
