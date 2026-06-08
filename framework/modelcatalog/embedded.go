package modelcatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

//go:embed datasheet/pricing.json
var embeddedPricingJSON []byte

//go:embed datasheet/model-parameters.json
var embeddedModelParametersJSON []byte

// EmbeddedPricingEntries returns pricing entries baked into the binary.
func EmbeddedPricingEntries() (map[string]PricingEntry, error) {
	return parseEmbeddedPricing()
}

func parseEmbeddedPricing() (map[string]PricingEntry, error) {
	var pricingData map[string]PricingEntry
	if err := json.Unmarshal(embeddedPricingJSON, &pricingData); err != nil {
		return nil, fmt.Errorf("failed to parse embedded pricing data: %w", err)
	}
	return pricingData, nil
}

func parseEmbeddedModelParameters() (map[string]json.RawMessage, error) {
	var paramsData map[string]json.RawMessage
	if err := json.Unmarshal(embeddedModelParametersJSON, &paramsData); err != nil {
		return nil, fmt.Errorf("failed to parse embedded model parameters data: %w", err)
	}
	return paramsData, nil
}

func (mc *ModelCatalog) loadEmbeddedCatalog() error {
	pricingData, err := parseEmbeddedPricing()
	if err != nil {
		return err
	}

	paramsData, err := parseEmbeddedModelParameters()
	if err != nil {
		return err
	}

	mc.mu.Lock()
	mc.pricingData = make(map[string]configstoreTables.TableModelPricing, len(pricingData))
	seen := make(map[string]bool, len(pricingData))
	for modelKey, entry := range pricingData {
		pricing := convertPricingDataToTableModelPricing(modelKey, entry)
		key := makeKey(pricing.Model, pricing.Provider, pricing.Mode)
		if seen[key] {
			continue
		}
		seen[key] = true
		mc.pricingData[key] = pricing
	}
	mc.mu.Unlock()

	mc.populateModelParamsFromPricing(pricingData)
	mc.applyModelParameters(paramsData)

	mc.modelParametersMu.Lock()
	mc.modelParametersData = paramsData
	mc.modelParametersMu.Unlock()

	mc.logger.Info("loaded embedded model catalog: %d pricing records, %d model parameter records", len(pricingData), len(paramsData))
	return nil
}
