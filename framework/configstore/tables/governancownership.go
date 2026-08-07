package tables

import (
	"fmt"
	"sort"
	"strings"
)

// budgetOwnerFields counts how many parent FK columns are set on a budget row.
func budgetOwnerFields(b *TableBudget) (fields []string) {
	if b == nil {
		return nil
	}
	if b.VirtualKeyID != nil {
		fields = append(fields, "virtual_key_id")
	}
	if b.ProviderConfigID != nil {
		fields = append(fields, "provider_config_id")
	}
	if b.TeamID != nil {
		fields = append(fields, "team_id")
	}
	if b.ProviderID != nil {
		fields = append(fields, "provider_id")
	}
	if b.ModelConfigID != nil {
		fields = append(fields, "model_config_id")
	}
	if b.GovernedOrganizationID != nil {
		fields = append(fields, "governed_organization_id")
	}
	return fields
}

func validateBudgetOwner(b *TableBudget) error {
	fields := budgetOwnerFields(b)
	if len(fields) > 1 {
		return fmt.Errorf("budget cannot have more than one owner (%v)", fields)
	}
	return nil
}

func rateLimitOwnerFields(rl *TableRateLimit) (fields []string) {
	if rl == nil {
		return nil
	}
	if rl.VirtualKeyID != nil {
		fields = append(fields, "virtual_key_id")
	}
	if rl.ProviderConfigID != nil {
		fields = append(fields, "provider_config_id")
	}
	if rl.ProviderID != nil {
		fields = append(fields, "provider_id")
	}
	if rl.ModelConfigID != nil {
		fields = append(fields, "model_config_id")
	}
	if rl.GovernedOrganizationID != nil {
		fields = append(fields, "governed_organization_id")
	}
	return fields
}

func validateRateLimitOwner(rl *TableRateLimit) error {
	fields := rateLimitOwnerFields(rl)
	if len(fields) > 1 {
		return fmt.Errorf("rate limit cannot have more than one owner (%v)", fields)
	}
	return nil
}

// BudgetRefID returns the budget ID when a relationship is loaded.
func BudgetRefID(b *TableBudget) string {
	if b == nil {
		return ""
	}
	return b.ID
}

// RateLimitRefID returns the rate limit ID when a relationship is loaded.
func RateLimitRefID(rl *TableRateLimit) string {
	if rl == nil {
		return ""
	}
	return rl.ID
}

// JoinBudgetRefIDs returns a stable, comma-separated list of budget IDs for hashing.
func JoinBudgetRefIDs(budgets []TableBudget) string {
	if len(budgets) == 0 {
		return ""
	}
	ids := make([]string, 0, len(budgets))
	for i := range budgets {
		if budgets[i].ID != "" {
			ids = append(ids, budgets[i].ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// JoinRateLimitRefIDs returns a stable, comma-separated list of rate limit IDs for hashing.
func JoinRateLimitRefIDs(rateLimits []TableRateLimit) string {
	if len(rateLimits) == 0 {
		return ""
	}
	ids := make([]string, 0, len(rateLimits))
	for i := range rateLimits {
		if rateLimits[i].ID != "" {
			ids = append(ids, rateLimits[i].ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
