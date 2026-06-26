package logging

import (
	"context"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

const configModelPricingCacheTTL = 30 * time.Second

type cachedConfigModelPricing struct {
	index    *configstore.ConfigModelPricingIndex
	loadedAt time.Time
}

type configModelPricingCache struct {
	registry tenantstore.Resolver
	caches   sync.Map // tenantID -> *cachedConfigModelPricing
}

func newConfigModelPricingCache(registry tenantstore.Resolver) *configModelPricingCache {
	if registry == nil {
		return nil
	}
	return &configModelPricingCache{registry: registry}
}

func (c *configModelPricingCache) indexForContext(ctx context.Context) *configstore.ConfigModelPricingIndex {
	if c == nil || c.registry == nil {
		return nil
	}
	tenantID, _ := ctx.Value(schemas.BifrostContextKeyTenantID).(string)
	if cached, ok := c.caches.Load(tenantID); ok {
		if entry, ok := cached.(*cachedConfigModelPricing); ok && entry != nil &&
			time.Since(entry.loadedAt) < configModelPricingCacheTTL {
			return entry.index
		}
	}
	store := c.registry.GetStoreFromContext(ctx)
	if store == nil {
		return nil
	}
	index, err := configstore.BuildConfigModelPricingIndex(ctx, store)
	if err != nil {
		return nil
	}
	c.caches.Store(tenantID, &cachedConfigModelPricing{
		index:    index,
		loadedAt: time.Now(),
	})
	return index
}

func (c *configModelPricingCache) calculateCost(
	ctx context.Context,
	provider, resolvedModel, aliasModel string,
	promptTokens, completionTokens int,
) float64 {
	index := c.indexForContext(ctx)
	if index == nil {
		return 0
	}
	return index.CalculateTokenCost(provider, resolvedModel, aliasModel, promptTokens, completionTokens)
}

func (c *configModelPricingCache) lookupCurrencyID(
	ctx context.Context,
	provider, resolvedModel, aliasModel string,
) string {
	index := c.indexForContext(ctx)
	if index == nil {
		return ""
	}
	return index.LookupCurrencyID(provider, resolvedModel, aliasModel)
}

func aliasFromEntry(entry *logstore.Log) string {
	if entry == nil || entry.Alias == nil {
		return ""
	}
	return *entry.Alias
}

func tokenCountsFromEntry(entry *logstore.Log) (promptTokens, completionTokens int) {
	if entry == nil {
		return 0, 0
	}
	if entry.TokenUsageParsed != nil {
		return entry.TokenUsageParsed.PromptTokens, entry.TokenUsageParsed.CompletionTokens
	}
	return entry.PromptTokens, entry.CompletionTokens
}

func (p *LoggerPlugin) applyConfigModelCostToEntry(ctx *schemas.BifrostContext, entry *logstore.Log) {
	if p == nil || entry == nil || p.configModelPricing == nil {
		return
	}
	promptTokens, completionTokens := tokenCountsFromEntry(entry)
	if promptTokens == 0 && completionTokens == 0 {
		return
	}
	provider := string(entry.Provider)
	model := entry.Model
	alias := aliasFromEntry(entry)
	cost := p.configModelPricing.calculateCost(ctx, provider, model, alias, promptTokens, completionTokens)
	if cost > 0 {
		entry.Cost = &cost
		if currencyID := p.configModelPricing.lookupCurrencyID(ctx, provider, model, alias); currencyID != "" {
			entry.CostCurrencyID = &currencyID
		}
	}
}
