package configstore

import "github.com/maximhq/bifrost/framework/configstore/tables"

// AttachGovernanceFromReverseFK hydrates parent entities from ownership columns on
// budget and rate limit rows (provider_id, model_config_id, virtual_key_id, etc.).
func AttachGovernanceFromReverseFK(
	budgets []tables.TableBudget,
	rateLimits []tables.TableRateLimit,
	providers []tables.TableProvider,
	configModels []tables.TableModel,
	virtualKeys []tables.TableVirtualKey,
) {
	providerByID := make(map[string]*tables.TableProvider, len(providers))
	for i := range providers {
		providerByID[providers[i].ID] = &providers[i]
	}

	configModelByID := make(map[string]*tables.TableModel, len(configModels))
	for i := range configModels {
		configModelByID[configModels[i].ID] = &configModels[i]
	}

	vkByID := make(map[string]*tables.TableVirtualKey, len(virtualKeys))
	for i := range virtualKeys {
		vkByID[virtualKeys[i].ID] = &virtualKeys[i]
	}

	providerConfigByID := make(map[string]*tables.TableVirtualKeyProviderConfig)
	for i := range virtualKeys {
		for j := range virtualKeys[i].ProviderConfigs {
			pc := &virtualKeys[i].ProviderConfigs[j]
			providerConfigByID[pc.ID] = pc
		}
	}

	for i := range budgets {
		b := &budgets[i]
		switch {
		case b.ProviderID != nil:
			if p, ok := providerByID[*b.ProviderID]; ok {
				p.Budgets = appendUniqueBudget(p.Budgets, b)
			}
		case b.ModelConfigID != nil:
			if cm, ok := configModelByID[*b.ModelConfigID]; ok {
				cm.Budgets = appendUniqueBudget(cm.Budgets, b)
			}
		case b.VirtualKeyID != nil:
			if vk, ok := vkByID[*b.VirtualKeyID]; ok {
				vk.Budgets = appendUniqueBudget(vk.Budgets, b)
			}
		case b.ProviderConfigID != nil:
			if pc, ok := providerConfigByID[*b.ProviderConfigID]; ok {
				pc.Budgets = appendUniqueBudget(pc.Budgets, b)
			}
		}
	}

	for i := range rateLimits {
		rl := &rateLimits[i]
		switch {
		case rl.ProviderID != nil:
			if p, ok := providerByID[*rl.ProviderID]; ok {
				p.RateLimits = appendUniqueRateLimit(p.RateLimits, rl)
			}
		case rl.ModelConfigID != nil:
			if cm, ok := configModelByID[*rl.ModelConfigID]; ok {
				cm.RateLimits = appendUniqueRateLimit(cm.RateLimits, rl)
			}
		case rl.VirtualKeyID != nil:
			if vk, ok := vkByID[*rl.VirtualKeyID]; ok {
				vk.RateLimits = appendUniqueRateLimit(vk.RateLimits, rl)
			}
		case rl.ProviderConfigID != nil:
			if pc, ok := providerConfigByID[*rl.ProviderConfigID]; ok {
				pc.RateLimits = appendUniqueRateLimit(pc.RateLimits, rl)
			}
		}
	}
}

func appendUniqueBudget(existing []tables.TableBudget, b *tables.TableBudget) []tables.TableBudget {
	for i := range existing {
		if existing[i].ID == b.ID {
			return existing
		}
	}
	return append(existing, *b)
}

func appendUniqueRateLimit(existing []tables.TableRateLimit, rl *tables.TableRateLimit) []tables.TableRateLimit {
	for i := range existing {
		if existing[i].ID == rl.ID {
			return existing
		}
	}
	return append(existing, *rl)
}
