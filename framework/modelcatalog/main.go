// Package modelcatalog provides a pricing manager for the framework.
package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

type ModelCatalog struct {
	configStore configstore.ConfigStore
	logger      schemas.Logger

	// In-memory cache for fast access - direct map for O(1) lookups
	pricingData map[string]configstoreTables.TableModelPricing
	mu          sync.RWMutex

	// rawOverrides is the canonical list of all active overrides. It exists solely
	// to support incremental mutations: UpsertPricingOverrides and DeletePricingOverride
	// iterate over it to rebuild the list, then derive customPricing from it.
	// customPricing is the actual lookup structure used at query time.
	rawOverrides  []PricingOverride
	customPricing *customPricingData
	overridesMu   sync.RWMutex

	modelPool           map[schemas.ModelProvider][]string
	unfilteredModelPool map[schemas.ModelProvider][]string // model pool without allowed models filtering
	baseModelIndex      map[string]string                  // model string → canonical base model name

	// Pre-parsed supported response types index (keyed by model name)
	// Values are normalized response types: "chat_completion", "responses", "text_completion"
	supportedResponseTypes map[string][]string

	// Pre-parsed supported parameters index (keyed by model name, populated from model parameters supported_parameters)
	// Values are parameter names the model accepts (e.g., "temperature", "top_p", "tools")
	supportedParams map[string][]string

	// Embedded model-parameter JSON keyed by model name (global, not tenant-scoped).
	modelParametersData map[string]json.RawMessage
	modelParametersMu   sync.RWMutex
}

// Init initializes the global in-memory model catalog from embedded datasheet data.
func Init(ctx context.Context, _ *Config, configStore configstore.ConfigStore, logger schemas.Logger) (*ModelCatalog, error) {
	mc := &ModelCatalog{
		configStore:            configStore,
		logger:                 logger,
		pricingData:            make(map[string]configstoreTables.TableModelPricing),
		modelPool:              make(map[schemas.ModelProvider][]string),
		unfilteredModelPool:    make(map[schemas.ModelProvider][]string),
		baseModelIndex:         make(map[string]string),
		supportedResponseTypes: make(map[string][]string),
		supportedParams:        make(map[string][]string),
		modelParametersData:    make(map[string]json.RawMessage),
	}

	logger.Info("initializing model catalog from embedded datasheet...")
	if err := mc.loadEmbeddedCatalog(); err != nil {
		return nil, fmt.Errorf("failed to load embedded model catalog: %w", err)
	}

	mc.populateModelPoolFromPricingData()

	if err := mc.loadPricingOverridesFromStore(ctx); err != nil {
		return nil, fmt.Errorf("failed to load pricing overrides: %w", err)
	}

	providerUtils.SetCacheMissHandler(func(model string) *providerUtils.ModelParams {
		return mc.lookupModelParams(model)
	})

	return mc, nil
}

func (mc *ModelCatalog) lookupModelParams(model string) *providerUtils.ModelParams {
	raw, ok := mc.GetModelParametersJSON(model)
	if !ok {
		return nil
	}
	var p struct {
		MaxOutputTokens       *int  `json:"max_output_tokens"`
		VertexMultiRegionOnly *bool `json:"vertex_multi_region_only"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	if p.MaxOutputTokens == nil && p.VertexMultiRegionOnly == nil {
		return nil
	}
	return &providerUtils.ModelParams{
		MaxOutputTokens:         p.MaxOutputTokens,
		IsVertexMultiRegionOnly: p.VertexMultiRegionOnly,
	}
}

// GetModelParametersJSON returns the raw embedded parameter JSON for a model.
func (mc *ModelCatalog) GetModelParametersJSON(model string) (json.RawMessage, bool) {
	if mc == nil {
		return nil, false
	}
	mc.modelParametersMu.RLock()
	defer mc.modelParametersMu.RUnlock()
	raw, ok := mc.modelParametersData[model]
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out, true
}

// SetShouldSyncGate is retained for API compatibility; remote catalog sync is disabled.
func (mc *ModelCatalog) SetShouldSyncGate(_ func(ctx context.Context) bool) {}

// SetAfterSyncHook is retained for API compatibility; remote catalog sync is disabled.
func (mc *ModelCatalog) SetAfterSyncHook(_ func(ctx context.Context)) {}

// WaitStartupBackgroundSync is retained for API compatibility.
func (mc *ModelCatalog) WaitStartupBackgroundSync(_ context.Context) {}

// ReloadFromDB reloads the embedded catalog and tenant pricing overrides.
func (mc *ModelCatalog) ReloadFromDB(ctx context.Context) error {
	if err := mc.loadEmbeddedCatalog(); err != nil {
		return err
	}
	mc.populateModelPoolFromPricingData()
	return mc.loadPricingOverridesFromStore(ctx)
}

// UpdateSyncConfig reloads the embedded catalog and tenant pricing overrides.
func (mc *ModelCatalog) UpdateSyncConfig(ctx context.Context, _ *Config) error {
	return mc.ForceReloadPricing(ctx)
}

func (mc *ModelCatalog) ForceReloadPricing(ctx context.Context) error {
	if err := mc.loadEmbeddedCatalog(); err != nil {
		return fmt.Errorf("failed to reload embedded model catalog: %w", err)
	}
	mc.populateModelPoolFromPricingData()
	if err := mc.loadPricingOverridesFromStore(ctx); err != nil {
		return fmt.Errorf("failed to load pricing overrides: %w", err)
	}
	return nil
}

// IsRequestTypeSupported checks if a model supports chat completion.
// It checks the supportedResponseTypes index.
func (mc *ModelCatalog) IsRequestTypeSupported(model string, provider schemas.ModelProvider, requestType schemas.RequestType) bool {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	outputs, ok := mc.supportedResponseTypes[model]
	return ok && slices.Contains(outputs, string(requestType))
}

// GetSupportedParameters returns the list of supported parameter names for a model.
// Returns nil if the model is not found in the catalog.
func (mc *ModelCatalog) GetSupportedParameters(model string) []string {
	mc.mu.RLock()
	params, ok := mc.supportedParams[model]
	mc.mu.RUnlock()
	if !ok {
		return nil
	}
	result := make([]string, len(params))
	copy(result, params)
	return result
}

// populateModelPool populates the model pool with all available models per provider (thread-safe)
func (mc *ModelCatalog) populateModelPoolFromPricingData() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.modelPool = make(map[schemas.ModelProvider][]string)
	mc.unfilteredModelPool = make(map[schemas.ModelProvider][]string)
	mc.baseModelIndex = make(map[string]string)

	providerModels := make(map[schemas.ModelProvider]map[string]bool)

	for _, pricing := range mc.pricingData {
		normalizedProvider := schemas.ModelProvider(normalizeProvider(pricing.Provider))

		if providerModels[normalizedProvider] == nil {
			providerModels[normalizedProvider] = make(map[string]bool)
		}

		providerModels[normalizedProvider][pricing.Model] = true

		if pricing.BaseModel != "" {
			mc.baseModelIndex[pricing.Model] = pricing.BaseModel
		}
	}

	for provider, modelSet := range providerModels {
		models := make([]string, 0, len(modelSet))
		for model := range modelSet {
			models = append(models, model)
		}
		mc.modelPool[provider] = models
		mc.unfilteredModelPool[provider] = models
	}

	totalModels := 0
	for provider, models := range mc.modelPool {
		totalModels += len(models)
		mc.logger.Debug("populated %d models for provider %s", len(models), string(provider))
	}
	mc.logger.Info("populated model pool with %d models across %d providers", totalModels, len(mc.modelPool))
}

func (mc *ModelCatalog) Cleanup() error {
	return nil
}

// NewTestCatalog creates a minimal ModelCatalog for testing purposes.
func NewTestCatalog(baseModelIndex map[string]string) *ModelCatalog {
	if baseModelIndex == nil {
		baseModelIndex = make(map[string]string)
	}
	return &ModelCatalog{
		modelPool:              make(map[schemas.ModelProvider][]string),
		unfilteredModelPool:    make(map[schemas.ModelProvider][]string),
		baseModelIndex:         baseModelIndex,
		pricingData:            make(map[string]configstoreTables.TableModelPricing),
		supportedResponseTypes: make(map[string][]string),
		supportedParams:        make(map[string][]string),
		modelParametersData:    make(map[string]json.RawMessage),
	}
}
