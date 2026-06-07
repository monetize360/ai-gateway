package tenantstore

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

type modelCatalogSource interface {
	GetModelPrices(ctx context.Context) ([]configstoreTables.TableModelPricing, error)
	GetModelParameters(ctx context.Context) ([]configstoreTables.TableModelParameters, error)
}

type modelCatalogTarget interface {
	ExecuteTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error
	UpsertModelPrices(ctx context.Context, pricing *configstoreTables.TableModelPricing, tx ...*gorm.DB) error
	UpsertModelParameters(ctx context.Context, params *configstoreTables.TableModelParameters, tx ...*gorm.DB) error
}

func replicateModelPricingRows(
	ctx context.Context,
	target modelCatalogTarget,
	pricingRows []configstoreTables.TableModelPricing,
	tx *gorm.DB,
) error {
	seen := make(map[string]bool, len(pricingRows))
	for i := range pricingRows {
		pricing := pricingRows[i]
		key := pricing.Model + "|" + pricing.Provider + "|" + pricing.Mode
		if seen[key] {
			continue
		}
		seen[key] = true
		row := pricing
		if err := target.UpsertModelPrices(ctx, &row, tx); err != nil {
			return fmt.Errorf("failed to upsert pricing for model %s: %w", row.Model, err)
		}
	}
	return nil
}

func replicateModelParameterRows(
	ctx context.Context,
	target modelCatalogTarget,
	paramRows []configstoreTables.TableModelParameters,
	tx *gorm.DB,
) error {
	for i := range paramRows {
		row := paramRows[i]
		if err := target.UpsertModelParameters(ctx, &row, tx); err != nil {
			return fmt.Errorf("failed to upsert parameters for model %s: %w", row.Model, err)
		}
	}
	return nil
}

// ReplicateModelPricingData copies governance_model_pricing rows from source into target.
func ReplicateModelPricingData(
	ctx context.Context,
	source modelCatalogSource,
	target modelCatalogTarget,
) error {
	if source == nil || target == nil {
		return fmt.Errorf("source and target config stores are required")
	}
	pricingRows, err := source.GetModelPrices(ctx)
	if err != nil {
		return fmt.Errorf("failed to read model pricing from source: %w", err)
	}
	if len(pricingRows) == 0 {
		return nil
	}
	return target.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		return replicateModelPricingRows(ctx, target, pricingRows, tx)
	})
}

// ReplicateModelParametersData copies governance_model_parameters rows from source into target.
func ReplicateModelParametersData(
	ctx context.Context,
	source modelCatalogSource,
	target modelCatalogTarget,
) error {
	if source == nil || target == nil {
		return fmt.Errorf("source and target config stores are required")
	}
	paramRows, err := source.GetModelParameters(ctx)
	if err != nil {
		return fmt.Errorf("failed to read model parameters from source: %w", err)
	}
	if len(paramRows) == 0 {
		return nil
	}
	return target.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		return replicateModelParameterRows(ctx, target, paramRows, tx)
	})
}

// ReplicateModelCatalogData copies governance_model_pricing and
// governance_model_parameters rows from source into target using upsert semantics.
func ReplicateModelCatalogData(
	ctx context.Context,
	source modelCatalogSource,
	target modelCatalogTarget,
	logger schemas.Logger,
) error {
	if source == nil || target == nil {
		return fmt.Errorf("source and target config stores are required")
	}

	pricingRows, err := source.GetModelPrices(ctx)
	if err != nil {
		return fmt.Errorf("failed to read model pricing from source: %w", err)
	}
	paramRows, err := source.GetModelParameters(ctx)
	if err != nil {
		return fmt.Errorf("failed to read model parameters from source: %w", err)
	}
	if len(pricingRows) == 0 && len(paramRows) == 0 {
		return nil
	}

	return target.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		if err := replicateModelPricingRows(ctx, target, pricingRows, tx); err != nil {
			return err
		}
		return replicateModelParameterRows(ctx, target, paramRows, tx)
	})
}

func syncModelCatalogRowsToAllTenants(
	ctx context.Context,
	manager *TenantDBManager,
	logger schemas.Logger,
	replicate func(context.Context, modelCatalogTarget, *gorm.DB) error,
	kind string,
) {
	if manager == nil || replicate == nil {
		return
	}
	if err := manager.SyncTenantsFromGlobalDB(ctx); err != nil {
		logger.Warn("tenant model catalog %s sync: failed to refresh tenant registry: %v", kind, err)
	}

	tenantIDs := manager.ListTenantIDs()
	if len(tenantIDs) == 0 {
		return
	}

	synced := 0
	for _, tenantID := range tenantIDs {
		store, err := manager.GetStore(ctx, tenantID)
		if err != nil || store == nil {
			logger.Warn("tenant model catalog %s sync: no config store for tenant %s", kind, tenantID)
			continue
		}
		target, ok := store.(modelCatalogTarget)
		if !ok {
			logger.Warn("tenant model catalog %s sync: config store for tenant %s does not support catalog replication", kind, tenantID)
			continue
		}
		if err := target.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			return replicate(ctx, target, tx)
		}); err != nil {
			logger.Warn("tenant model catalog %s sync failed for tenant %s: %v", kind, tenantID, err)
			continue
		}
		synced++
	}
	if synced > 0 {
		logger.Info("tenant model catalog %s sync completed for %d/%d tenant(s)", kind, synced, len(tenantIDs))
	}
}

// SyncModelPricingRowsToAllTenants upserts pricing rows into every tenant DB.
func SyncModelPricingRowsToAllTenants(
	ctx context.Context,
	manager *TenantDBManager,
	pricingRows []configstoreTables.TableModelPricing,
	logger schemas.Logger,
) {
	if len(pricingRows) == 0 {
		return
	}
	syncModelCatalogRowsToAllTenants(ctx, manager, logger, func(ctx context.Context, target modelCatalogTarget, tx *gorm.DB) error {
		return replicateModelPricingRows(ctx, target, pricingRows, tx)
	}, "pricing")
}

// SyncModelParameterRowsToAllTenants upserts model parameter rows into every tenant DB.
func SyncModelParameterRowsToAllTenants(
	ctx context.Context,
	manager *TenantDBManager,
	paramRows []configstoreTables.TableModelParameters,
	logger schemas.Logger,
) {
	if len(paramRows) == 0 {
		return
	}
	syncModelCatalogRowsToAllTenants(ctx, manager, logger, func(ctx context.Context, target modelCatalogTarget, tx *gorm.DB) error {
		return replicateModelParameterRows(ctx, target, paramRows, tx)
	}, "parameters")
}

// SyncModelCatalogFromSource copies model pricing and parameters from source into every tenant DB.
func SyncModelCatalogFromSource(
	ctx context.Context,
	manager *TenantDBManager,
	source modelCatalogSource,
	logger schemas.Logger,
) {
	if manager == nil || source == nil {
		return
	}
	pricingRows, err := source.GetModelPrices(ctx)
	if err != nil {
		logger.Warn("tenant model catalog sync: failed to read pricing from source: %v", err)
	}
	paramRows, err := source.GetModelParameters(ctx)
	if err != nil {
		logger.Warn("tenant model catalog sync: failed to read parameters from source: %v", err)
	}
	SyncModelPricingRowsToAllTenants(ctx, manager, pricingRows, logger)
	SyncModelParameterRowsToAllTenants(ctx, manager, paramRows, logger)
}
