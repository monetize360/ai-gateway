package modelcatalog

import "time"

const (
	DefaultSyncInterval           = 24 * time.Hour
	MinimumPricingSyncIntervalSec = int64(3600)

	ConfigLastPricingSyncKey = "LastModelPricingSync"
	ConfigLastParamsSyncKey  = "LastModelParametersSync"
)

// Config is retained for API compatibility. Catalog data is loaded from embedded datasheets.
type Config struct {
	PricingURL          *string `json:"pricing_url,omitempty"`
	PricingSyncInterval *int64  `json:"pricing_sync_interval,omitempty"` // seconds
	ModelParametersURL  *string `json:"model_parameters_url,omitempty"`
}
