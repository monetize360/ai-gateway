// Package handlers provides HTTP request handlers for the Bifrost HTTP transport.
// This file contains all governance management functionality including CRUD operations for VKs, Rules, and configs.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// dbForUpdate adds a PostgreSQL row-level update lock to the query.
func dbForUpdate(db *gorm.DB) *gorm.DB {
	if db.Dialector.Name() != "postgres" {
		return db
	}
	return db.Clauses(clause.Locking{Strength: "UPDATE"})
}

// GovernanceManager is the interface for the governance manager
type GovernanceManager interface {
	GetGovernanceData(ctx context.Context) *governance.GovernanceData
	ReloadVirtualKey(ctx context.Context, id string) (*configstoreTables.TableVirtualKey, error)
	RemoveVirtualKey(ctx context.Context, id string) error
	ReloadTeam(ctx context.Context, id string) (*configstoreTables.TableTeam, error)
	RemoveTeam(ctx context.Context, id string) error
	ReloadCustomer(ctx context.Context, id string) (*configstoreTables.TableCustomer, error)
	RemoveCustomer(ctx context.Context, id string) error
	ReloadModelConfig(ctx context.Context, id string) (*configstoreTables.TableModelConfig, error)
	RemoveModelConfig(ctx context.Context, id string) error
	ReloadProvider(ctx context.Context, provider schemas.ModelProvider) (*configstoreTables.TableProvider, error)
	RemoveProvider(ctx context.Context, provider schemas.ModelProvider) error
	ReloadRoutingRule(ctx context.Context, id string) error
	RemoveRoutingRule(ctx context.Context, id string) error
}

// GovernanceHandler manages HTTP requests for governance operations
type GovernanceHandler struct {
	cfg               *lib.Config
	governanceManager GovernanceManager
}

// NewGovernanceHandler creates a new governance handler instance
func NewGovernanceHandler(manager GovernanceManager, cfg *lib.Config) (*GovernanceHandler, error) {
	if manager == nil {
		return nil, fmt.Errorf("governance manager is required")
	}
	if cfg == nil || cfg.TenantStore == nil {
		return nil, fmt.Errorf("tenant store is required")
	}
	return &GovernanceHandler{
		governanceManager: manager,
		cfg:               cfg,
	}, nil
}

// CreateVirtualKeyRequest represents the request body for creating a virtual key
type CreateVirtualKeyRequest struct {
	Name            string `json:"name" validate:"required"`
	Description     string `json:"description,omitempty"`
	AllowedModelConfigs []struct {
		Provider          string                  `json:"provider" validate:"required"`
		Weight            *float64                `json:"weight,omitempty"`
		AllowedModels     schemas.WhiteList       `json:"allowed_models,omitempty"`     // ["*"] allows all models; empty denies all
		BlacklistedModels schemas.BlackList       `json:"blacklisted_models,omitempty"` // ["*"] blocks all models; empty blocks none
		Budgets           []CreateBudgetRequest   `json:"budgets,omitempty"`            // Multi-budget for provider config
		RateLimits        []CreateRateLimitRequest `json:"rate_limits,omitempty"`
		KeyIDs            schemas.WhiteList       `json:"key_ids,omitempty"`            // List of DBKey UUIDs to associate with this provider config
	} `json:"allowed_model_configs,omitempty"` // Empty means all providers allowed
	MCPConfigs []struct {
		MCPClientName  string            `json:"mcp_client_name" validate:"required"`
		ToolsToExecute schemas.WhiteList `json:"tools_to_execute,omitempty"`
	} `json:"mcp_configs,omitempty"` // Empty means no MCP clients allowed (deny-by-default)
	OrgID           *string                  `json:"org_id,omitempty"`
	ScopeOrgID      *string                  `json:"scope_org_id,omitempty"`
	Budgets         []CreateBudgetRequest    `json:"budgets,omitempty"`     // Multi-budget: each must have a unique reset_duration
	RateLimits      []CreateRateLimitRequest `json:"rate_limits,omitempty"`
	IsActive        *bool                    `json:"is_active,omitempty"`
	CalendarAligned bool                    `json:"calendar_aligned,omitempty"` // When true, all budgets reset at clean calendar boundaries
}

// UpdateVirtualKeyRequest represents the request body for updating a virtual key
type UpdateVirtualKeyRequest struct {
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	AllowedModelConfigs []struct {
		ID                *string                 `json:"id,omitempty"` // null for new entries
		Provider          string                  `json:"provider" validate:"required"`
		Weight            *float64                `json:"weight,omitempty"`
		AllowedModels     schemas.WhiteList       `json:"allowed_models,omitempty"`     // ["*"] allows all models; empty denies all
		BlacklistedModels schemas.BlackList       `json:"blacklisted_models,omitempty"` // ["*"] blocks all models; empty blocks none
		Budgets           []CreateBudgetRequest   `json:"budgets,omitempty"`            // Multi-budget for provider config
		RateLimits        []CreateRateLimitRequest  `json:"rate_limits,omitempty"`
		KeyIDs            schemas.WhiteList       `json:"key_ids,omitempty"`            // List of DBKey UUIDs to associate with this provider config
	} `json:"allowed_model_configs,omitempty"`
	MCPConfigs []struct {
		ID             *string           `json:"id,omitempty"` // null for new entries
		MCPClientName  string            `json:"mcp_client_name" validate:"required"`
		ToolsToExecute schemas.WhiteList `json:"tools_to_execute,omitempty"`
	} `json:"mcp_configs,omitempty"`
	OrgID            *string                 `json:"org_id,omitempty"`
	ScopeOrgID       *string                 `json:"scope_org_id,omitempty"`
	Budgets          []CreateBudgetRequest   `json:"budgets,omitempty"` // Multi-budget: replaces all VK-level budgets
	RateLimits       []CreateRateLimitRequest  `json:"rate_limits,omitempty"`
	IsActive         *bool                   `json:"is_active,omitempty"`
	CalendarAligned  *bool                   `json:"calendar_aligned,omitempty"` // When true, all budgets reset at clean calendar boundaries
	ResetBudgetUsage *bool                   `json:"reset_budget_usage,omitempty"`
}

type BulkRotateVirtualKeysRequest struct {
	IDs []string `json:"ids"`
}

// CreateBudgetRequest represents the request body for creating a budget
type CreateBudgetRequest struct {
	ID            string  `json:"id,omitempty"`
	MaxLimit      float64 `json:"max_limit" validate:"required"`      // Maximum budget in dollars
	ResetDuration string  `json:"reset_duration" validate:"required"` // e.g., "30s", "5m", "1h", "1d", "1w", "1M"
	SoftLimit     *bool   `json:"soft_limit,omitempty"`               // When true, requests pass through even when budget is exhausted
}

// UpdateBudgetRequest represents the request body for updating a budget
type UpdateBudgetRequest struct {
	MaxLimit      *float64 `json:"max_limit,omitempty"`
	ResetDuration *string  `json:"reset_duration,omitempty"`
	SoftLimit     *bool    `json:"soft_limit,omitempty"` // When true, requests pass through even when budget is exhausted
}

// CreateRoutingRuleRequest represents the request body for creating a routing rule
type CreateRoutingRuleRequest struct {
	Name          string         `json:"name" validate:"required"`
	Description   string         `json:"description,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`    // nil = use DB default (true)
	ChainRule     *bool          `json:"chain_rule,omitempty"` // nil = use DB default (false)
	CelExpression string         `json:"cel_expression"`
	ProviderID    *string        `json:"provider_id,omitempty"` // nil = use incoming provider
	ModelID       *string        `json:"model_id,omitempty"`    // nil = use incoming model
	Provider      *string        `json:"provider,omitempty"`    // config/UI alias; resolved to provider_id on save
	Model         *string        `json:"model,omitempty"`       // config/UI alias; resolved to model_id on save
	KeyID         *string        `json:"key_id,omitempty"`      // nil = no key pin
	Fallbacks     []string       `json:"fallbacks,omitempty"`
	ScopeOrgID    *string        `json:"scope_org_id,omitempty"`
	VirtualKeyID  *string        `json:"virtual_key_id,omitempty"`
	Query         map[string]any `json:"query,omitempty"`
	Priority      int            `json:"priority,omitempty"` // Defaults to 0 if not provided
}

// UpdateRoutingRuleRequest represents the request body for updating a routing rule
type UpdateRoutingRuleRequest struct {
	Name          *string        `json:"name,omitempty"`
	Description   *string        `json:"description,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
	ChainRule     *bool          `json:"chain_rule,omitempty"`
	CelExpression *string        `json:"cel_expression,omitempty"`
	ProviderID    *string        `json:"provider_id,omitempty"`
	ModelID       *string        `json:"model_id,omitempty"`
	Provider      *string        `json:"provider,omitempty"`
	Model         *string        `json:"model,omitempty"`
	KeyID         *string        `json:"key_id,omitempty"`
	Fallbacks     []string       `json:"fallbacks,omitempty"`
	Query         map[string]any `json:"query,omitempty"`
	Priority      *int           `json:"priority,omitempty"`
	ScopeOrgID    *string        `json:"scope_org_id,omitempty"`
	VirtualKeyID  *string        `json:"virtual_key_id,omitempty"`
}

// CreateRateLimitRequest represents the request body for creating a rate limit using flexible approach
type CreateRateLimitRequest struct {
	ID                   string  `json:"id,omitempty"`
	TokenMaxLimit        *int64  `json:"token_max_limit,omitempty"`        // Maximum tokens allowed
	TokenResetDuration   *string `json:"token_reset_duration,omitempty"`   // e.g., "30s", "5m", "1h", "1d", "1w", "1M"
	RequestMaxLimit      *int64  `json:"request_max_limit,omitempty"`      // Maximum requests allowed
	RequestResetDuration *string `json:"request_reset_duration,omitempty"` // e.g., "30s", "5m", "1h", "1d", "1w", "1M"
	SoftLimit            *bool   `json:"soft_limit,omitempty"`             // When true, requests pass through even when rate limit is exhausted
}

// UpdateRateLimitRequest represents the request body for updating a rate limit using flexible approach
type UpdateRateLimitRequest struct {
	TokenMaxLimit        *int64  `json:"token_max_limit,omitempty"`        // Maximum tokens allowed
	TokenResetDuration   *string `json:"token_reset_duration,omitempty"`   // e.g., "30s", "5m", "1h", "1d", "1w", "1M"
	RequestMaxLimit      *int64  `json:"request_max_limit,omitempty"`      // Maximum requests allowed
	RequestResetDuration *string `json:"request_reset_duration,omitempty"` // e.g., "30s", "5m", "1h", "1d", "1w", "1M"
	SoftLimit            *bool   `json:"soft_limit,omitempty"`             // When true, requests pass through even when rate limit is exhausted
}

func isBudgetRemovalRequest(req *UpdateBudgetRequest) bool {
	return req != nil && req.MaxLimit == nil && req.ResetDuration == nil && req.SoftLimit == nil
}

// budgetLastReset returns the appropriate LastReset for a new budget.
// When calendarAligned is true it snaps to the start of the current calendar period
// (e.g. midnight on the 1st of the month for "1M"), otherwise it returns time.Now().
func budgetLastReset(calendarAligned bool, resetDuration string) time.Time {
	if calendarAligned && configstoreTables.IsCalendarAlignableDuration(resetDuration) {
		return configstoreTables.GetCalendarPeriodStart(resetDuration, time.Now())
	}
	return time.Now()
}

func resetBudgetUsageIfRequested(budget *configstoreTables.TableBudget, reset bool, calendarAligned bool) {
	if !reset {
		return
	}
	budget.CurrentUsage = 0
	budget.LastReset = budgetLastReset(calendarAligned, budget.ResetDuration)
}

func compareBudgetRequestDurations(left, right CreateBudgetRequest) bool {
	leftDuration, leftErr := configstoreTables.ParseDuration(left.ResetDuration)
	rightDuration, rightErr := configstoreTables.ParseDuration(right.ResetDuration)
	if leftErr == nil && rightErr == nil && leftDuration != rightDuration {
		return leftDuration < rightDuration
	}
	return left.ResetDuration < right.ResetDuration
}

func inheritUsageFromClosestShorterBudget(budget *configstoreTables.TableBudget, existing []configstoreTables.TableBudget, reset bool) {
	if reset {
		return
	}
	targetDuration, err := configstoreTables.ParseDuration(budget.ResetDuration)
	if err != nil {
		return
	}

	var closest *configstoreTables.TableBudget
	var closestDuration time.Duration
	for i := range existing {
		candidate := &existing[i]
		candidateDuration, err := configstoreTables.ParseDuration(candidate.ResetDuration)
		if err != nil || candidateDuration >= targetDuration {
			continue
		}
		if closest == nil || candidateDuration > closestDuration {
			closest = candidate
			closestDuration = candidateDuration
		}
	}
	if closest == nil {
		return
	}
	budget.CurrentUsage = closest.CurrentUsage
}

// buildBudgetLookup builds ID- and duration-keyed maps of existing budgets for
// the reconciliation pass. Rows whose ID is explicitly claimed by an
// ID-specified entry in requests are omitted from byDuration so a
// duration-only entry that sorts earlier cannot steal the row reserved for an
// ID-based rename (e.g. payload [{new 1d}, {ID:X→1w}] against existing
// {ID:X, "1d"}).
func buildBudgetLookup(existing []configstoreTables.TableBudget, requests []CreateBudgetRequest) (map[string]configstoreTables.TableBudget, map[string]configstoreTables.TableBudget) {
	claimedIDs := make(map[string]struct{}, len(requests))
	for _, r := range requests {
		if r.ID != "" {
			claimedIDs[r.ID] = struct{}{}
		}
	}
	byID := make(map[string]configstoreTables.TableBudget, len(existing))
	byDuration := make(map[string]configstoreTables.TableBudget, len(existing))
	for _, budget := range existing {
		if budget.ID != "" {
			byID[budget.ID] = budget
		}
		if _, claimed := claimedIDs[budget.ID]; claimed {
			continue
		}
		byDuration[budget.ResetDuration] = budget
	}
	return byID, byDuration
}

func findExistingBudget(request CreateBudgetRequest, byID map[string]configstoreTables.TableBudget, byDuration map[string]configstoreTables.TableBudget) (configstoreTables.TableBudget, bool, error) {
	if request.ID != "" {
		existing, found := byID[request.ID]
		if !found {
			return configstoreTables.TableBudget{}, false, &badRequestError{err: fmt.Errorf("budget %s does not belong to this entity", request.ID)}
		}
		// Consume the matched row from both maps so a later iteration cannot
		// reuse it (e.g. renaming an existing budget by ID while the same
		// payload adds a new budget with the old duration).
		delete(byID, existing.ID)
		delete(byDuration, existing.ResetDuration)
		return existing, true, nil
	}
	existing, found := byDuration[request.ResetDuration]
	if found {
		delete(byID, existing.ID)
		delete(byDuration, existing.ResetDuration)
	}
	return existing, found, nil
}

func isRateLimitRemovalRequest(req *UpdateRateLimitRequest) bool {
	return req != nil && req.TokenMaxLimit == nil && req.RequestMaxLimit == nil &&
		req.TokenResetDuration == nil && req.RequestResetDuration == nil && req.SoftLimit == nil
}

func rateLimitRequestKey(req CreateRateLimitRequest) string {
	td, rd := "", ""
	if req.TokenResetDuration != nil {
		td = *req.TokenResetDuration
	}
	if req.RequestResetDuration != nil {
		rd = *req.RequestResetDuration
	}
	return td + "|" + rd
}

func rateLimitKeyFromTable(rl configstoreTables.TableRateLimit) string {
	td, rd := "", ""
	if rl.TokenResetDuration != nil {
		td = *rl.TokenResetDuration
	}
	if rl.RequestResetDuration != nil {
		rd = *rl.RequestResetDuration
	}
	return td + "|" + rd
}

func reconcileRateLimitRequests(
	ctx context.Context,
	store configstore.ConfigStore,
	tx *gorm.DB,
	existing []configstoreTables.TableRateLimit,
	requests []CreateRateLimitRequest,
	assignOwner func(*configstoreTables.TableRateLimit),
) ([]configstoreTables.TableRateLimit, error) {
	byID := make(map[string]configstoreTables.TableRateLimit, len(existing))
	byKey := make(map[string]configstoreTables.TableRateLimit, len(existing))
	for _, rl := range existing {
		byID[rl.ID] = rl
		byKey[rateLimitKeyFromTable(rl)] = rl
	}
	seenKeys := make(map[string]bool, len(requests))
	reconciled := make([]configstoreTables.TableRateLimit, 0, len(requests))
	matchedIDs := make(map[string]bool, len(existing))
	for _, req := range requests {
		key := rateLimitRequestKey(req)
		if seenKeys[key] {
			return nil, fmt.Errorf("duplicate rate limit window: %s", key)
		}
		seenKeys[key] = true

		var rl configstoreTables.TableRateLimit
		found := false
		if req.ID != "" {
			rl, found = byID[req.ID]
			if !found {
				return nil, fmt.Errorf("rate limit %s does not belong to this entity", req.ID)
			}
		} else if existingRL, ok := byKey[key]; ok {
			rl, found = existingRL, true
		}
		if found {
			rl.TokenMaxLimit = req.TokenMaxLimit
			rl.TokenResetDuration = req.TokenResetDuration
			rl.RequestMaxLimit = req.RequestMaxLimit
			rl.RequestResetDuration = req.RequestResetDuration
			if req.SoftLimit != nil {
				rl.SoftLimit = req.SoftLimit
			}
			if err := validateRateLimit(&rl); err != nil {
				return nil, err
			}
			if err := store.UpdateRateLimit(ctx, &rl, tx); err != nil {
				return nil, err
			}
			reconciled = append(reconciled, rl)
			matchedIDs[rl.ID] = true
			continue
		}
		rl = configstoreTables.TableRateLimit{
			ID:                   uuid.NewString(),
			TokenMaxLimit:        req.TokenMaxLimit,
			TokenResetDuration:   req.TokenResetDuration,
			RequestMaxLimit:      req.RequestMaxLimit,
			RequestResetDuration: req.RequestResetDuration,
			SoftLimit:            req.SoftLimit,
			TokenLastReset:       time.Now(),
			RequestLastReset:     time.Now(),
		}
		assignOwner(&rl)
		if err := validateRateLimit(&rl); err != nil {
			return nil, err
		}
		if err := store.CreateRateLimit(ctx, &rl, tx); err != nil {
			return nil, err
		}
		reconciled = append(reconciled, rl)
	}
	for _, old := range existing {
		if !matchedIDs[old.ID] {
			if err := store.DeleteRateLimit(ctx, old.ID, tx); err != nil {
				if !errors.Is(err, configstore.ErrNotFound) {
					return nil, fmt.Errorf("failed to delete removed rate limit: %w", err)
				}
			}
		}
	}
	return reconciled, nil
}

func collectProviderConfigDeleteIDs(
	config configstoreTables.TableAllowedModelConfig,
	budgetIDs []string,
	rateLimitIDs []string,
) ([]string, []string) {
	for _, b := range config.Budgets {
		budgetIDs = append(budgetIDs, b.ID)
	}
	for _, rl := range config.RateLimits {
		rateLimitIDs = append(rateLimitIDs, rl.ID)
	}
	return budgetIDs, rateLimitIDs
}

// CreateTeamRequest represents the request body for creating a team
type CreateTeamRequest struct {
	Name            string                  `json:"name" validate:"required"`
	CustomerID      *string                 `json:"customer_id,omitempty"`      // Team can belong to a customer
	Budgets         []CreateBudgetRequest   `json:"budgets,omitempty"`          // Multi-budget: each must have a unique reset_duration
	RateLimit       *CreateRateLimitRequest `json:"rate_limit,omitempty"`       // Team can have its own rate limit
	CalendarAligned bool                    `json:"calendar_aligned,omitempty"` // Team-wide: snap all team budgets and rate-limit resets to calendar boundaries
}

// UpdateTeamRequest represents the request body for updating a team
type UpdateTeamRequest struct {
	Name            *string                 `json:"name,omitempty"`
	CustomerID      *string                 `json:"customer_id,omitempty"`
	Budgets         []CreateBudgetRequest   `json:"budgets,omitempty"` // Multi-budget: replaces all team budgets
	RateLimit       *UpdateRateLimitRequest `json:"rate_limit,omitempty"`
	CalendarAligned *bool                   `json:"calendar_aligned,omitempty"` // Team-wide setting; nil means "leave unchanged"
}

// CreateCustomerRequest represents the request body for creating a customer
type CreateCustomerRequest struct {
	Name      string                  `json:"name" validate:"required"`
	Budget    *CreateBudgetRequest    `json:"budget,omitempty"`
	RateLimit *CreateRateLimitRequest `json:"rate_limit,omitempty"` // Customer can have its own rate limit
}

// UpdateCustomerRequest represents the request body for updating a customer
type UpdateCustomerRequest struct {
	Name      *string                 `json:"name,omitempty"`
	Budget    *UpdateBudgetRequest    `json:"budget,omitempty"`
	RateLimit *UpdateRateLimitRequest `json:"rate_limit,omitempty"`
}

// CreateModelConfigRequest represents the request body for creating a model config
type CreateModelConfigRequest struct {
	ModelName string                  `json:"model_name" validate:"required"`
	Provider  *string                 `json:"provider,omitempty"` // Optional provider, nil means all providers
	Budgets    []CreateBudgetRequest    `json:"budgets,omitempty"`
	RateLimits []CreateRateLimitRequest `json:"rate_limits,omitempty"`
}

// UpdateModelConfigRequest represents the request body for updating a model config
type UpdateModelConfigRequest struct {
	ModelName  *string                  `json:"model_name,omitempty"`
	Provider   *string                  `json:"provider,omitempty"` // Optional provider, nil means no change
	Budgets    []CreateBudgetRequest    `json:"budgets,omitempty"`
	RateLimits []CreateRateLimitRequest `json:"rate_limits,omitempty"`
}

// UpdateProviderGovernanceRequest represents the request body for updating provider governance
type UpdateProviderGovernanceRequest struct {
	Budgets    []CreateBudgetRequest    `json:"budgets,omitempty"`
	RateLimits []CreateRateLimitRequest `json:"rate_limits,omitempty"`
}

// RegisterRoutes registers all governance-related routes for the new hierarchical system
func (h *GovernanceHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	// Virtual Key CRUD operations
	r.GET("/api/governance/virtual-keys", lib.ChainMiddlewares(h.getVirtualKeys, middlewares...))
	r.POST("/api/governance/virtual-keys", lib.ChainMiddlewares(h.createVirtualKey, middlewares...))
	r.POST("/api/governance/virtual-keys/rotate", lib.ChainMiddlewares(h.rotateVirtualKeys, middlewares...))
	r.GET("/api/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.getVirtualKey, middlewares...))
	r.PUT("/api/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.updateVirtualKey, middlewares...))
	r.POST("/api/governance/virtual-keys/{vk_id}/rotate", lib.ChainMiddlewares(h.rotateVirtualKey, middlewares...))
	r.DELETE("/api/governance/virtual-keys/{vk_id}", lib.ChainMiddlewares(h.deleteVirtualKey, middlewares...))

	// Team CRUD operations
	r.GET("/api/governance/teams", lib.ChainMiddlewares(h.getTeams, middlewares...))
	r.POST("/api/governance/teams", lib.ChainMiddlewares(h.createTeam, middlewares...))
	r.GET("/api/governance/teams/{team_id}", lib.ChainMiddlewares(h.getTeam, middlewares...))
	r.PUT("/api/governance/teams/{team_id}", lib.ChainMiddlewares(h.updateTeam, middlewares...))
	r.DELETE("/api/governance/teams/{team_id}", lib.ChainMiddlewares(h.deleteTeam, middlewares...))

	// Customer CRUD operations
	r.GET("/api/governance/customers", lib.ChainMiddlewares(h.getCustomers, middlewares...))
	r.POST("/api/governance/customers", lib.ChainMiddlewares(h.createCustomer, middlewares...))
	r.GET("/api/governance/customers/{customer_id}", lib.ChainMiddlewares(h.getCustomer, middlewares...))
	r.PUT("/api/governance/customers/{customer_id}", lib.ChainMiddlewares(h.updateCustomer, middlewares...))
	r.DELETE("/api/governance/customers/{customer_id}", lib.ChainMiddlewares(h.deleteCustomer, middlewares...))

	// Budget and Rate Limit GET operations
	r.GET("/api/governance/budgets", lib.ChainMiddlewares(h.getBudgets, middlewares...))
	r.GET("/api/governance/rate-limits", lib.ChainMiddlewares(h.getRateLimits, middlewares...))

	// Routing Rules CRUD operations
	r.GET("/api/governance/routing-rules", lib.ChainMiddlewares(h.getRoutingRules, middlewares...))
	r.POST("/api/governance/routing-rules", lib.ChainMiddlewares(h.createRoutingRule, middlewares...))
	r.GET("/api/governance/routing-rules/{rule_id}", lib.ChainMiddlewares(h.getRoutingRule, middlewares...))
	r.PUT("/api/governance/routing-rules/{rule_id}", lib.ChainMiddlewares(h.updateRoutingRule, middlewares...))
	r.DELETE("/api/governance/routing-rules/{rule_id}", lib.ChainMiddlewares(h.deleteRoutingRule, middlewares...))

	// Model Config CRUD operations
	r.GET("/api/governance/model-configs", lib.ChainMiddlewares(h.getModelConfigs, middlewares...))
	r.POST("/api/governance/model-configs", lib.ChainMiddlewares(h.createModelConfig, middlewares...))
	r.GET("/api/governance/model-configs/{mc_id}", lib.ChainMiddlewares(h.getModelConfig, middlewares...))
	r.PUT("/api/governance/model-configs/{mc_id}", lib.ChainMiddlewares(h.updateModelConfig, middlewares...))
	r.DELETE("/api/governance/model-configs/{mc_id}", lib.ChainMiddlewares(h.deleteModelConfig, middlewares...))

	// Provider Governance operations
	r.GET("/api/governance/providers", lib.ChainMiddlewares(h.getProviderGovernance, middlewares...))
	r.PUT("/api/governance/providers/{provider_name}", lib.ChainMiddlewares(h.updateProviderGovernance, middlewares...))
	r.DELETE("/api/governance/providers/{provider_name}", lib.ChainMiddlewares(h.deleteProviderGovernance, middlewares...))

	// Self-service endpoint — no admin auth, VK in header is the credential.
	// Registered without admin middlewares; only common middlewares (telemetry) are applied.
	r.GET("/api/governance/virtual-keys/quota", h.getVirtualKeyQuota)
}

// Virtual Key CRUD Operations

// getVirtualKeys handles GET /api/governance/virtual-keys - Get all virtual keys with relationships
func (h *GovernanceHandler) getVirtualKeys(ctx *fasthttp.RequestCtx) {
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		// Convert map to slice to match the non-memory response format (array)
		virtualKeys := make([]*configstoreTables.TableVirtualKey, 0, len(data.VirtualKeys))
		for _, vk := range data.VirtualKeys {
			virtualKeys = append(virtualKeys, vk)
		}
		sort.Slice(virtualKeys, func(i, j int) bool {
			return virtualKeys[i].CreatedAt.Before(virtualKeys[j].CreatedAt)
		})
		SendJSON(ctx, map[string]interface{}{
			"virtual_keys": virtualKeys,
			"count":        len(virtualKeys),
			"total_count":  len(virtualKeys),
			"limit":        len(virtualKeys),
			"offset":       0,
		})
		return
	}
	// Check for pagination/filter parameters
	limitStr := string(ctx.QueryArgs().Peek("limit"))
	offsetStr := string(ctx.QueryArgs().Peek("offset"))
	search := string(ctx.QueryArgs().Peek("search"))
	orgID := string(ctx.QueryArgs().Peek("org_id"))
	sortBy := string(ctx.QueryArgs().Peek("sort_by"))
	order := string(ctx.QueryArgs().Peek("order"))
	isExport := string(ctx.QueryArgs().Peek("export")) == "true"
	excludeAccessProfileManagedVirtual := string(ctx.QueryArgs().Peek("exclude_access_profile_managed_virtual")) == "true"

	if limitStr != "" || offsetStr != "" || search != "" || orgID != "" || sortBy != "" || isExport || excludeAccessProfileManagedVirtual {
		// Paginated/filtered path
		params := configstore.VirtualKeyQueryParams{
			Search:                             search,
			OrgID:                              orgID,
			SortBy:                             sortBy,
			Order:                              order,
			Export:                             isExport,
			ExcludeAccessProfileManagedVirtual: excludeAccessProfileManagedVirtual,
		}
		if limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil {
				SendError(ctx, 400, "Invalid limit parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid limit parameter: must be non-negative")
				return
			}
			params.Limit = n
		}
		if offsetStr != "" {
			n, err := strconv.Atoi(offsetStr)
			if err != nil {
				SendError(ctx, 400, "Invalid offset parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid offset parameter: must be non-negative")
				return
			}
			params.Offset = n
		}

		if !params.Export {
			params.Limit, params.Offset = ClampPaginationParams(params.Limit, params.Offset)
		} else if params.Offset < 0 {
			params.Offset = 0
		}
		virtualKeys, totalCount, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKeysPaginated(ctx, params)
		if err != nil {
			logger.Error("failed to retrieve virtual keys: %v", err)
			SendError(ctx, 500, "Failed to retrieve virtual keys")
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"virtual_keys": virtualKeys,
			"count":        len(virtualKeys),
			"total_count":  totalCount,
			"limit":        params.Limit,
			"offset":       params.Offset,
		})
		return
	}

	// Non-paginated path: return all virtual keys
	virtualKeys, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKeys(ctx)
	if err != nil {
		logger.Error("failed to retrieve virtual keys: %v", err)
		SendError(ctx, 500, "Failed to retrieve virtual keys")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"virtual_keys": virtualKeys,
		"count":        len(virtualKeys),
		"total_count":  len(virtualKeys),
		"limit":        len(virtualKeys),
		"offset":       0,
	})
}

// createVirtualKey handles POST /api/governance/virtual-keys - Create a new virtual key
func (h *GovernanceHandler) createVirtualKey(ctx *fasthttp.RequestCtx) {
	var req CreateVirtualKeyRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Validate required fields
	if req.Name == "" {
		SendError(ctx, 400, "Virtual key name is required")
		return
	}
	// Validate budgets if provided
	if len(req.Budgets) > 0 {
		seenDurations := make(map[string]bool)
		for _, b := range req.Budgets {
			if b.MaxLimit < 0 {
				SendError(ctx, 400, fmt.Sprintf("Budget max_limit cannot be negative: %.2f", b.MaxLimit))
				return
			}
			if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
				SendError(ctx, 400, fmt.Sprintf("Invalid reset duration format: %s", b.ResetDuration))
				return
			}
			if seenDurations[b.ResetDuration] {
				SendError(ctx, 400, fmt.Sprintf("Duplicate reset_duration in budgets: %s", b.ResetDuration))
				return
			}
			seenDurations[b.ResetDuration] = true
		}
	}
	// Set defaults: nil means "use DB default (true)"
	isActive := req.IsActive
	if isActive == nil {
		isActive = bifrost.Ptr(true)
	}
	// Fetch providers from DB to ensure up-to-date data in cluster mode.
	providerSet := map[schemas.ModelProvider]struct{}{}
	if req.AllowedModelConfigs != nil {
		var err error
		providerSet, err = h.getConfiguredProviderSet(ctx)
		if err != nil {
			SendError(ctx, 500, fmt.Sprintf("Failed to load providers: %v", err))
			return
		}
	}
	var vk configstoreTables.TableVirtualKey
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		vk = configstoreTables.TableVirtualKey{
			ID:              uuid.NewString(),
			Name:            req.Name,
			Value:           governance.GenerateVirtualKey(),
			Description:     req.Description,
			OrgID:           req.OrgID,
			ScopeOrgID:      req.ScopeOrgID,
			IsActive:        isActive,
			CalendarAligned: req.CalendarAligned,
		}
		if err := h.cfg.StoreFromRequestCtx(ctx).CreateVirtualKey(ctx, &vk, tx); err != nil {
			return err
		}
		if len(req.RateLimits) > 0 {
			vkID := vk.ID
			reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, nil, req.RateLimits, func(rl *configstoreTables.TableRateLimit) {
				rl.VirtualKeyID = &vkID
			})
			if err != nil {
				return err
			}
			vk.RateLimits = reconciled
		}
		// Create multi-budgets for VK
		if len(req.Budgets) > 0 {
			for _, b := range req.Budgets {
				budget := configstoreTables.TableBudget{
					ID:            uuid.NewString(),
					MaxLimit:      b.MaxLimit,
					ResetDuration: b.ResetDuration,
					SoftLimit:     b.SoftLimit,
					LastReset:     budgetLastReset(vk.CalendarAligned, b.ResetDuration),
					CurrentUsage:  0,
					VirtualKeyID:  &vk.ID,
				}
				if err := validateBudget(&budget); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
					return err
				}
			}
		}
		if req.AllowedModelConfigs != nil {
			for _, pc := range req.AllowedModelConfigs {
				providerName := schemas.ModelProvider(strings.TrimSpace(pc.Provider))
				if providerName == "" {
					return &badRequestError{err: fmt.Errorf("provider name is required")}
				}
				if _, ok := providerSet[providerName]; !ok {
					return &badRequestError{err: fmt.Errorf("invalid provider name: %s", pc.Provider)}
				}
				if err := pc.AllowedModels.Validate(); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid allowed_models for provider %s: %w", pc.Provider, err)}
				}
				if err := pc.BlacklistedModels.Validate(); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid blacklisted_models for provider %s: %w", pc.Provider, err)}
				}
				if err := pc.KeyIDs.Validate(); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid key_ids for provider %s: %w", pc.Provider, err)}
				}

				// Get keys for this provider config if specified
				var keys []configstoreTables.TableKey
				allowAllKeys := true
				if !pc.KeyIDs.IsUnrestricted() && !pc.KeyIDs.IsEmpty() {
					allowAllKeys = false
					var err error
					keys, err = h.cfg.StoreFromRequestCtx(ctx).GetKeysByIDs(ctx, pc.KeyIDs)
					if err != nil {
						return fmt.Errorf("failed to get keys by IDs for provider %s: %w", pc.Provider, err)
					}
					if len(keys) != len(pc.KeyIDs) {
						return fmt.Errorf("some keys not found for provider %s: expected %d, found %d", pc.Provider, len(pc.KeyIDs), len(keys))
					}
				}

			providerConfig := &configstoreTables.TableAllowedModelConfig{
				VirtualKeyID:      &vk.ID,
				Provider:          string(providerName),
				AllowedModels:     pc.AllowedModels,
				BlacklistedModels: pc.BlacklistedModels,
				AllowAllKeys:      allowAllKeys,
				Keys:              keys,
			}

			if err := h.cfg.StoreFromRequestCtx(ctx).CreateAllowedModelConfigExpanded(ctx, providerConfig, tx); err != nil {
				return err
			}

			if len(pc.RateLimits) > 0 {
				pcID := providerConfig.ID
				reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, nil, pc.RateLimits, func(rl *configstoreTables.TableRateLimit) {
					rl.ProviderConfigID = &pcID
				})
				if err != nil {
					return err
				}
				providerConfig.RateLimits = reconciled
			}
			// Create multi-budgets for provider config
			if len(pc.Budgets) > 0 {
					seenDurations := make(map[string]bool)
					for _, b := range pc.Budgets {
						if seenDurations[b.ResetDuration] {
							return &badRequestError{err: fmt.Errorf("duplicate reset_duration in provider config budgets: %s", b.ResetDuration)}
						}
						seenDurations[b.ResetDuration] = true
						budget := configstoreTables.TableBudget{
							ID:               uuid.NewString(),
							MaxLimit:         b.MaxLimit,
							ResetDuration:    b.ResetDuration,
							SoftLimit:        b.SoftLimit,
							LastReset:        budgetLastReset(vk.CalendarAligned, b.ResetDuration),
							CurrentUsage:     0,
							ProviderConfigID: &providerConfig.ID,
						}
						if err := validateBudget(&budget); err != nil {
							return err
						}
						if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
							return err
						}
					}
				}
			}
		}
		if req.MCPConfigs != nil {
			// Check for duplicate MCPClientName values before processing
			seenMCPClientNames := make(map[string]bool)
			for _, mc := range req.MCPConfigs {
				if seenMCPClientNames[mc.MCPClientName] {
					return &badRequestError{err: fmt.Errorf("duplicate mcp_client_name: %s", mc.MCPClientName)}
				}
				seenMCPClientNames[mc.MCPClientName] = true
			}

			for _, mc := range req.MCPConfigs {
				if err := mc.ToolsToExecute.Validate(); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid tools_to_execute for mcp client %s: %w", mc.MCPClientName, err)}
				}
				mcpClient, err := h.cfg.StoreFromRequestCtx(ctx).GetMCPClientByName(ctx, mc.MCPClientName)
				if err != nil {
					return fmt.Errorf("failed to get MCP client: %w", err)
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateVirtualKeyMCPConfig(ctx, &configstoreTables.TableVirtualKeyMCPConfig{
					VirtualKeyID:   vk.ID,
					MCPClientID:    mcpClient.ID,
					ToolsToExecute: mc.ToolsToExecute,
				}, tx); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		var badReqErr *badRequestError
		if errors.As(err, &badReqErr) {
			SendError(ctx, 400, err.Error())
			return
		}
		SendError(ctx, 500, err.Error())
		return
	}
	preloadedVk, err := h.governanceManager.ReloadVirtualKey(ctx, vk.ID)
	if err != nil {
		logger.Error("failed to reload virtual key: %v", err)
		preloadedVk = &vk
	}

	SendJSON(ctx, map[string]any{
		"message":     "Virtual key created successfully",
		"virtual_key": preloadedVk,
	})
}

// getVirtualKey handles GET /api/governance/virtual-keys/{vk_id} - Get a specific virtual key
func (h *GovernanceHandler) getVirtualKey(ctx *fasthttp.RequestCtx) {
	vkID := ctx.UserValue("vk_id").(string)
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		for _, vk := range data.VirtualKeys {
			if vk.ID == vkID {
				SendJSON(ctx, map[string]interface{}{
					"virtual_key": vk,
				})
				return
			}
		}
		SendError(ctx, 404, "Virtual key not found")
		return
	}
	vk, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKey(ctx, vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Virtual key not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve virtual key")
		return
	}

	SendJSON(ctx, map[string]interface{}{
		"virtual_key": vk,
	})
}

// updateVirtualKey handles PUT /api/governance/virtual-keys/{vk_id} - Update a virtual key
func (h *GovernanceHandler) updateVirtualKey(ctx *fasthttp.RequestCtx) {
	vkID := ctx.UserValue("vk_id").(string)
	var req UpdateVirtualKeyRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	vk, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKey(ctx, vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Virtual key not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve virtual key")
		return
	}
	providerSet := map[schemas.ModelProvider]struct{}{}
	if len(req.AllowedModelConfigs) > 0 {
		providerSet, err = h.getConfiguredProviderSet(ctx)
		if err != nil {
			SendError(ctx, 500, fmt.Sprintf("Failed to load providers: %v", err))
			return
		}
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		var providerBudgetIDsToDelete []string
		var providerRateLimitIDsToDelete []string
		var lockedVK configstoreTables.TableVirtualKey
		if err := dbForUpdate(tx.WithContext(ctx)).
			Preload("Budgets").
			Preload("RateLimits").
			Preload("AllowedModelConfigs").
			First(&lockedVK, "id = ?", vkID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return configstore.ErrNotFound
			}
			return err
		}
		vk = &lockedVK
		sort.Slice(vk.Budgets, func(i, j int) bool {
			if vk.Budgets[i].ResetDuration == vk.Budgets[j].ResetDuration {
				return vk.Budgets[i].ID < vk.Budgets[j].ID
			}
			return vk.Budgets[i].ResetDuration < vk.Budgets[j].ResetDuration
		})

		// Update fields if provided
		if req.Name != nil {
			vk.Name = *req.Name
		}
		if req.Description != nil {
			vk.Description = *req.Description
		}
		if req.OrgID != nil {
			if strings.TrimSpace(*req.OrgID) == "" {
				vk.OrgID = nil
			} else {
				vk.OrgID = req.OrgID
			}
		}
		if req.ScopeOrgID != nil {
			if strings.TrimSpace(*req.ScopeOrgID) == "" {
				vk.ScopeOrgID = nil
			} else {
				vk.ScopeOrgID = req.ScopeOrgID
			}
		}
		if req.IsActive != nil {
			vk.IsActive = req.IsActive
		}
		if req.CalendarAligned != nil {
			vk.CalendarAligned = *req.CalendarAligned
		}
		// Handle multi-budget updates
		if req.Budgets != nil {
			// Validate multi-budgets
			seenDurations := make(map[string]bool)
			requestBudgets := append([]CreateBudgetRequest(nil), req.Budgets...)
			sort.Slice(requestBudgets, func(i, j int) bool {
				return compareBudgetRequestDurations(requestBudgets[i], requestBudgets[j])
			})
			for _, b := range requestBudgets {
				if b.MaxLimit < 0 {
					return &badRequestError{err: fmt.Errorf("budget max_limit cannot be negative: %.2f", b.MaxLimit)}
				}
				if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid reset duration format: %s", b.ResetDuration)}
				}
				if seenDurations[b.ResetDuration] {
					return &badRequestError{err: fmt.Errorf("duplicate reset_duration in budgets: %s", b.ResetDuration)}
				}
				seenDurations[b.ResetDuration] = true
			}

			existingByID, existingByDuration := buildBudgetLookup(vk.Budgets, requestBudgets)
			resetBudgetUsage := req.ResetBudgetUsage != nil && *req.ResetBudgetUsage
			var reconciledBudgets []configstoreTables.TableBudget
			matchedIDs := make(map[string]bool)
			for _, b := range requestBudgets {
				existing, found, err := findExistingBudget(b, existingByID, existingByDuration)
				if err != nil {
					return err
				}
				if found {
					existing.MaxLimit = b.MaxLimit
					existing.ResetDuration = b.ResetDuration
					if b.SoftLimit != nil {
						existing.SoftLimit = b.SoftLimit
					}
					resetBudgetUsageIfRequested(&existing, resetBudgetUsage, vk.CalendarAligned)
					if err := validateBudget(&existing); err != nil {
						return err
					}
					if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, &existing, tx); err != nil {
						return err
					}
					reconciledBudgets = append(reconciledBudgets, existing)
					matchedIDs[existing.ID] = true
				} else {
					// New budget duration — create fresh
					budget := configstoreTables.TableBudget{
						ID:            uuid.NewString(),
						MaxLimit:      b.MaxLimit,
						ResetDuration: b.ResetDuration,
						SoftLimit:     b.SoftLimit,
						LastReset:     budgetLastReset(vk.CalendarAligned, b.ResetDuration),
						CurrentUsage:  0,
						VirtualKeyID:  &vk.ID,
					}
					inheritUsageFromClosestShorterBudget(&budget, vk.Budgets, resetBudgetUsage)
					if err := validateBudget(&budget); err != nil {
						return err
					}
					if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
						return err
					}
					reconciledBudgets = append(reconciledBudgets, budget)
				}
			}
			// Delete budgets that are no longer present
			for _, existing := range vk.Budgets {
				if !matchedIDs[existing.ID] {
					if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, existing.ID, tx); err != nil {
						return fmt.Errorf("failed to delete removed VK budget: %w", err)
					}
				}
			}
			vk.Budgets = reconciledBudgets
		}

		if req.RateLimits != nil {
			vkID := vk.ID
			reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, vk.RateLimits, req.RateLimits, func(rl *configstoreTables.TableRateLimit) {
				rl.VirtualKeyID = &vkID
			})
			if err != nil {
				return err
			}
			vk.RateLimits = reconciled
		}

		if err := h.cfg.StoreFromRequestCtx(ctx).UpdateVirtualKey(ctx, vk, tx); err != nil {
			return err
		}
		if req.AllowedModelConfigs != nil {
			// Get existing provider configs for comparison
			var existingConfigs []configstoreTables.TableAllowedModelConfig
			if err := tx.Where("virtual_key_id = ?", vk.ID).
				Preload("Budgets").
				Preload("RateLimits").
				Find(&existingConfigs).Error; err != nil {
				return err
			}
			sort.Slice(existingConfigs, func(i, j int) bool { return existingConfigs[i].ID < existingConfigs[j].ID })
			sort.Slice(req.AllowedModelConfigs, func(i, j int) bool {
				if req.AllowedModelConfigs[i].ID == nil && req.AllowedModelConfigs[j].ID != nil {
					return false
				}
				if req.AllowedModelConfigs[i].ID != nil && req.AllowedModelConfigs[j].ID == nil {
					return true
				}
				if req.AllowedModelConfigs[i].ID != nil && req.AllowedModelConfigs[j].ID != nil && *req.AllowedModelConfigs[i].ID != *req.AllowedModelConfigs[j].ID {
					return *req.AllowedModelConfigs[i].ID < *req.AllowedModelConfigs[j].ID
				}
				return req.AllowedModelConfigs[i].Provider < req.AllowedModelConfigs[j].Provider
			})
			// Create maps for easier lookup
			existingConfigsMap := make(map[string]configstoreTables.TableAllowedModelConfig)
			for _, config := range existingConfigs {
				existingConfigsMap[config.ID] = config
			}
			requestConfigsMap := make(map[string]bool)
			// Process new configs: create new ones and update existing ones
			for _, pc := range req.AllowedModelConfigs {
				providerName := schemas.ModelProvider(strings.TrimSpace(pc.Provider))
				if providerName == "" {
					return &badRequestError{err: fmt.Errorf("provider name is required")}
				}
				if _, ok := providerSet[providerName]; !ok {
					return &badRequestError{err: fmt.Errorf("invalid provider name: %s", pc.Provider)}
				}
				if pc.ID == nil {
					if err := pc.AllowedModels.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid allowed_models for provider %s: %w", pc.Provider, err)}
					}
					if err := pc.BlacklistedModels.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid blacklisted_models for provider %s: %w", pc.Provider, err)}
					}
					if err := pc.KeyIDs.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid key_ids for provider %s: %w", pc.Provider, err)}
					}

					// Get keys for this provider config if specified
					var keys []configstoreTables.TableKey
					allowAllKeys := true
					if !pc.KeyIDs.IsUnrestricted() && !pc.KeyIDs.IsEmpty() {
						allowAllKeys = false
						var err error
						keys, err = h.cfg.StoreFromRequestCtx(ctx).GetKeysByIDs(ctx, pc.KeyIDs)
						if err != nil {
							return fmt.Errorf("failed to get keys by IDs for provider %s: %w", pc.Provider, err)
						}
						if len(keys) != len(pc.KeyIDs) {
							return fmt.Errorf("some keys not found for provider %s: expected %d, found %d", pc.Provider, len(pc.KeyIDs), len(keys))
						}
					}

				// Create new provider config
				providerConfig := &configstoreTables.TableAllowedModelConfig{
					VirtualKeyID:      &vk.ID,
					Provider:          string(providerName),
					AllowedModels:     pc.AllowedModels,
					BlacklistedModels: pc.BlacklistedModels,
					AllowAllKeys:      allowAllKeys,
					Keys:              keys,
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateAllowedModelConfigExpanded(ctx, providerConfig, tx); err != nil {
					return err
				}
					if len(pc.RateLimits) > 0 {
						pcID := providerConfig.ID
						reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, nil, pc.RateLimits, func(rl *configstoreTables.TableRateLimit) {
							rl.ProviderConfigID = &pcID
						})
						if err != nil {
							return err
						}
						providerConfig.RateLimits = reconciled
					}
					// Create multi-budgets for new provider config in update
					if len(pc.Budgets) > 0 {
						seenDurations := make(map[string]bool)
						pcBudgets := append([]CreateBudgetRequest(nil), pc.Budgets...)
						sort.Slice(pcBudgets, func(i, j int) bool {
							return compareBudgetRequestDurations(pcBudgets[i], pcBudgets[j])
						})
						for _, b := range pcBudgets {
							if seenDurations[b.ResetDuration] {
								return &badRequestError{err: fmt.Errorf("duplicate reset_duration in provider config budgets: %s", b.ResetDuration)}
							}
							seenDurations[b.ResetDuration] = true
					budget := configstoreTables.TableBudget{
							ID:               uuid.NewString(),
							MaxLimit:         b.MaxLimit,
							ResetDuration:    b.ResetDuration,
							SoftLimit:        b.SoftLimit,
							LastReset:        budgetLastReset(vk.CalendarAligned, b.ResetDuration),
							CurrentUsage:     0,
							ProviderConfigID: &providerConfig.ID,
						}
							if err := validateBudget(&budget); err != nil {
								return err
							}
							if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
								return err
							}
						}
					}
				} else {
					// Update existing provider config
					existing, ok := existingConfigsMap[*pc.ID]
					if !ok {
						return fmt.Errorf("provider config %s does not belong to this virtual key", *pc.ID)
					}
					requestConfigsMap[*pc.ID] = true
					if err := pc.AllowedModels.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid allowed_models for provider %s: %w", pc.Provider, err)}
					}
					if err := pc.BlacklistedModels.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid blacklisted_models for provider %s: %w", pc.Provider, err)}
					}
					if err := pc.KeyIDs.Validate(); err != nil {
						return &badRequestError{err: fmt.Errorf("invalid key_ids for provider %s: %w", pc.Provider, err)}
					}
					existing.Provider = string(providerName)
					existing.Weight = pc.Weight
					existing.AllowedModels = pc.AllowedModels
					existing.BlacklistedModels = pc.BlacklistedModels

					// Get keys for this provider config if specified
					var keys []configstoreTables.TableKey
					allowAllKeys := true
					if !pc.KeyIDs.IsUnrestricted() && !pc.KeyIDs.IsEmpty() {
						allowAllKeys = false
						var err error
						keys, err = h.cfg.StoreFromRequestCtx(ctx).GetKeysByIDs(ctx, pc.KeyIDs)
						if err != nil {
							return fmt.Errorf("failed to get keys by IDs for provider %s: %w", pc.Provider, err)
						}
						if len(keys) != len(pc.KeyIDs) {
							return fmt.Errorf("some keys not found for provider %s: expected %d, found %d", pc.Provider, len(pc.KeyIDs), len(keys))
						}
					}
					existing.AllowAllKeys = allowAllKeys
					existing.Keys = keys

					// Handle multi-budget updates for existing provider config
					if pc.Budgets != nil {
						// Validate
						seenDurations := make(map[string]bool)
						pcBudgets := append([]CreateBudgetRequest(nil), pc.Budgets...)
						sort.Slice(pcBudgets, func(i, j int) bool {
							return compareBudgetRequestDurations(pcBudgets[i], pcBudgets[j])
						})
						for _, b := range pcBudgets {
							if b.MaxLimit < 0 {
								return &badRequestError{err: fmt.Errorf("provider config budget max_limit cannot be negative: %.2f", b.MaxLimit)}
							}
							if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
								return &badRequestError{err: fmt.Errorf("invalid provider config budget reset duration format: %s", b.ResetDuration)}
							}
							if seenDurations[b.ResetDuration] {
								return &badRequestError{err: fmt.Errorf("duplicate reset_duration in provider config budgets: %s", b.ResetDuration)}
							}
							seenDurations[b.ResetDuration] = true
						}

						sort.Slice(existing.Budgets, func(i, j int) bool {
							if existing.Budgets[i].ResetDuration == existing.Budgets[j].ResetDuration {
								return existing.Budgets[i].ID < existing.Budgets[j].ID
							}
							return existing.Budgets[i].ResetDuration < existing.Budgets[j].ResetDuration
						})

						pcExistingByID, pcExistingByDuration := buildBudgetLookup(existing.Budgets, pcBudgets)
						resetBudgetUsage := req.ResetBudgetUsage != nil && *req.ResetBudgetUsage
						var pcReconciledBudgets []configstoreTables.TableBudget
						pcMatchedIDs := make(map[string]bool)
						for _, b := range pcBudgets {
							eb, found, err := findExistingBudget(b, pcExistingByID, pcExistingByDuration)
							if err != nil {
								return err
							}
						if found {
							eb.MaxLimit = b.MaxLimit
							eb.ResetDuration = b.ResetDuration
							if b.SoftLimit != nil {
								eb.SoftLimit = b.SoftLimit
							}
							resetBudgetUsageIfRequested(&eb, resetBudgetUsage, vk.CalendarAligned)
							if err := validateBudget(&eb); err != nil {
								return err
							}
							if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, &eb, tx); err != nil {
								return err
							}
							pcReconciledBudgets = append(pcReconciledBudgets, eb)
							pcMatchedIDs[eb.ID] = true
						} else {
							// New budget duration — create fresh
							budget := configstoreTables.TableBudget{
								ID:               uuid.NewString(),
								MaxLimit:         b.MaxLimit,
								ResetDuration:    b.ResetDuration,
								SoftLimit:        b.SoftLimit,
								LastReset:        budgetLastReset(vk.CalendarAligned, b.ResetDuration),
								CurrentUsage:     0,
								ProviderConfigID: &existing.ID,
							}
								inheritUsageFromClosestShorterBudget(&budget, existing.Budgets, resetBudgetUsage)
								if err := validateBudget(&budget); err != nil {
									return err
								}
								if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
									return err
								}
								pcReconciledBudgets = append(pcReconciledBudgets, budget)
							}
						}
						// Delete budgets that are no longer present
						for _, eb := range existing.Budgets {
							if !pcMatchedIDs[eb.ID] {
								if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, eb.ID, tx); err != nil {
									return fmt.Errorf("failed to delete removed provider config budget: %w", err)
								}
							}
						}
						existing.Budgets = pcReconciledBudgets
					}
					if pc.RateLimits != nil {
						pcID := existing.ID
						reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, existing.RateLimits, pc.RateLimits, func(rl *configstoreTables.TableRateLimit) {
							rl.ProviderConfigID = &pcID
						})
						if err != nil {
							return err
						}
						existing.RateLimits = reconciled
					}
				if err := h.cfg.StoreFromRequestCtx(ctx).ReplaceAllowedModelConfigRows(ctx, &existing, tx); err != nil {
					return err
				}
			}
		}
		// Delete provider configs that are not in the request
			configIDs := make([]string, 0, len(existingConfigsMap))
			for id := range existingConfigsMap {
				configIDs = append(configIDs, id)
			}
			sort.Slice(configIDs, func(i, j int) bool { return configIDs[i] < configIDs[j] })
			for _, id := range configIDs {
				if !requestConfigsMap[id] {
					providerBudgetIDsToDelete, providerRateLimitIDsToDelete = collectProviderConfigDeleteIDs(
						existingConfigsMap[id],
						providerBudgetIDsToDelete,
						providerRateLimitIDsToDelete,
					)
					if err := h.cfg.StoreFromRequestCtx(ctx).DeleteAllowedModelConfig(ctx, id, tx); err != nil {
						return err
					}
				}
			}
		}
		if req.MCPConfigs != nil {
			// Check for duplicate MCPClientName values among all configs before processing
			seenMCPClientNames := make(map[string]bool)
			for _, mc := range req.MCPConfigs {
				if seenMCPClientNames[mc.MCPClientName] {
					return &badRequestError{err: fmt.Errorf("duplicate mcp_client_name: %s", mc.MCPClientName)}
				}
				seenMCPClientNames[mc.MCPClientName] = true
			}
			// Get existing MCP configs for comparison
			var existingMCPConfigs []configstoreTables.TableVirtualKeyMCPConfig
			if err := tx.Where("virtual_key_id = ?", vk.ID).Find(&existingMCPConfigs).Error; err != nil {
				return err
			}
			sort.Slice(existingMCPConfigs, func(i, j int) bool { return existingMCPConfigs[i].ID < existingMCPConfigs[j].ID })
			sort.Slice(req.MCPConfigs, func(i, j int) bool {
				if req.MCPConfigs[i].ID == nil && req.MCPConfigs[j].ID != nil {
					return false
				}
				if req.MCPConfigs[i].ID != nil && req.MCPConfigs[j].ID == nil {
					return true
				}
				if req.MCPConfigs[i].ID != nil && req.MCPConfigs[j].ID != nil && *req.MCPConfigs[i].ID != *req.MCPConfigs[j].ID {
					return *req.MCPConfigs[i].ID < *req.MCPConfigs[j].ID
				}
				return req.MCPConfigs[i].MCPClientName < req.MCPConfigs[j].MCPClientName
			})
			// Create maps for easier lookup
			existingMCPConfigsMap := make(map[string]configstoreTables.TableVirtualKeyMCPConfig)
			for _, config := range existingMCPConfigs {
				existingMCPConfigsMap[config.ID] = config
			}
			requestMCPConfigsMap := make(map[string]bool)
			// Process new configs: create new ones and update existing ones
			for _, mc := range req.MCPConfigs {
				if err := mc.ToolsToExecute.Validate(); err != nil {
					return &badRequestError{err: fmt.Errorf("invalid tools_to_execute for mcp client %s: %w", mc.MCPClientName, err)}
				}
				if mc.ID == nil {
					mcpClient, err := h.cfg.StoreFromRequestCtx(ctx).GetMCPClientByName(ctx, mc.MCPClientName)
					if err != nil {
						return fmt.Errorf("failed to get MCP client: %w", err)
					}
					// Create new MCP config
					if err := h.cfg.StoreFromRequestCtx(ctx).CreateVirtualKeyMCPConfig(ctx, &configstoreTables.TableVirtualKeyMCPConfig{
						VirtualKeyID:   vk.ID,
						MCPClientID:    mcpClient.ID,
						ToolsToExecute: mc.ToolsToExecute,
					}, tx); err != nil {
						return err
					}
				} else {
					// Update existing MCP config
					existing, ok := existingMCPConfigsMap[*mc.ID]
					if !ok {
						return fmt.Errorf("MCP config %s does not belong to this virtual key", *mc.ID)
					}
					requestMCPConfigsMap[*mc.ID] = true
					existing.ToolsToExecute = mc.ToolsToExecute
					if err := h.cfg.StoreFromRequestCtx(ctx).UpdateVirtualKeyMCPConfig(ctx, &existing, tx); err != nil {
						return err
					}
				}
			}
			// Delete MCP configs that are not in the request
			mcpConfigIDs := make([]string, 0, len(existingMCPConfigsMap))
			for id := range existingMCPConfigsMap {
				mcpConfigIDs = append(mcpConfigIDs, id)
			}
			sort.Slice(mcpConfigIDs, func(i, j int) bool { return mcpConfigIDs[i] < mcpConfigIDs[j] })
			for _, id := range mcpConfigIDs {
				if !requestMCPConfigsMap[id] {
					if err := h.cfg.StoreFromRequestCtx(ctx).DeleteVirtualKeyMCPConfig(ctx, id, tx); err != nil {
						return err
					}
				}
			}
		}

		sort.Strings(providerBudgetIDsToDelete)
		for _, id := range providerBudgetIDsToDelete {
			if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, id, tx); err != nil && !errors.Is(err, configstore.ErrNotFound) {
				return err
			}
		}
		sort.Strings(providerRateLimitIDsToDelete)
		for _, id := range providerRateLimitIDsToDelete {
			if err := h.cfg.StoreFromRequestCtx(ctx).DeleteRateLimit(ctx, id, tx); err != nil && !errors.Is(err, configstore.ErrNotFound) {
				return err
			}
		}

		return nil
	}); err != nil {
		var badReqErr *badRequestError
		if errors.As(err, &badReqErr) ||
			strings.Contains(err.Error(), "already exists") ||
			strings.Contains(err.Error(), "duplicate key") {
			SendError(ctx, 400, fmt.Sprintf("Failed to update virtual key: %v", err))
			return
		}
		SendError(ctx, 500, fmt.Sprintf("Failed to update virtual key: %v", err))
		return
	}
	// Load relationships for response
	preloadedVk, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKey(ctx, vk.ID)
	if err != nil {
		logger.Error("failed to load relationships for updated VK: %v", err)
		preloadedVk = vk
	}
	if _, err := h.governanceManager.ReloadVirtualKey(ctx, vk.ID); err != nil {
		// Should never happen but just in case
		logger.Error("failed to reload virtual key after update: %v", err)
		SendError(ctx, 500, "Virtual key updated in database but failed to reload in-memory state")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"message":     "Virtual key updated successfully",
		"virtual_key": preloadedVk,
	})
}

func (h *GovernanceHandler) rotateVirtualKeyByID(ctx context.Context, vkID string) (*configstoreTables.TableVirtualKey, error) {
	vk, err := h.cfg.StoreFromContext(ctx).GetVirtualKey(ctx, vkID)
	if err != nil {
		return nil, err
	}
	oldValue := vk.Value
	vk.Value = governance.GenerateVirtualKey()
	if vk.Value == oldValue {
		return nil, fmt.Errorf("generated virtual key matched existing value")
	}
	if err := h.cfg.StoreFromContext(ctx).UpdateVirtualKey(ctx, vk); err != nil {
		return nil, err
	}
	preloadedVk, err := h.governanceManager.ReloadVirtualKey(ctx, vk.ID)
	if err != nil {
		return nil, fmt.Errorf("virtual key rotated in database but failed to reload in-memory state: %w", err)
	}
	return preloadedVk, nil
}

// rotateVirtualKey handles POST /api/governance/virtual-keys/{vk_id}/rotate - Rotate only the virtual key value
func (h *GovernanceHandler) rotateVirtualKey(ctx *fasthttp.RequestCtx) {
	vkID := ctx.UserValue("vk_id").(string)
	preloadedVk, err := h.rotateVirtualKeyByID(ctx, vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Virtual key not found")
			return
		}
		logger.Error("failed to rotate virtual key: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to rotate virtual key: %v", err))
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"message":     "Virtual key rotated successfully",
		"virtual_key": preloadedVk,
	})
}

// rotateVirtualKeys handles POST /api/governance/virtual-keys/rotate - Rotate multiple virtual key values
func (h *GovernanceHandler) rotateVirtualKeys(ctx *fasthttp.RequestCtx) {
	var req BulkRotateVirtualKeysRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	if len(req.IDs) == 0 {
		SendError(ctx, 400, "At least one virtual key ID is required")
		return
	}

	seen := make(map[string]struct{}, len(req.IDs))
	ids := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			SendError(ctx, 400, "Virtual key ID cannot be empty")
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	rotated := make([]*configstoreTables.TableVirtualKey, 0, len(ids))
	failures := make(map[string]string)
	for _, id := range ids {
		vk, err := h.rotateVirtualKeyByID(ctx, id)
		if err != nil {
			if errors.Is(err, configstore.ErrNotFound) {
				failures[id] = "virtual key not found"
			} else {
				failures[id] = err.Error()
			}
			logger.Error("failed to rotate virtual key %s: %v", id, err)
			continue
		}
		rotated = append(rotated, vk)
	}

	response := map[string]interface{}{
		"message":      "Virtual keys rotated successfully",
		"virtual_keys": rotated,
	}
	if len(failures) > 0 {
		response["errors"] = failures
	}
	if len(rotated) == 0 {
		response["message"] = "Failed to rotate virtual keys"
		SendJSONWithStatus(ctx, response, 500)
		return
	}
	SendJSON(ctx, response)
}

// deleteVirtualKey handles DELETE /api/governance/virtual-keys/{vk_id} - Delete a virtual key
func (h *GovernanceHandler) deleteVirtualKey(ctx *fasthttp.RequestCtx) {
	vkID := ctx.UserValue("vk_id").(string)
	// Fetch the virtual key from the database to get the budget and rate limit
	vk, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKey(ctx, vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Virtual key not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve virtual key")
		return
	}
	// Deleting key from database
	if err := h.cfg.StoreFromRequestCtx(ctx).DeleteVirtualKey(ctx, vkID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Virtual key not found")
			return
		}
		logger.Error("failed to delete virtual key: %v", err)
		SendError(ctx, 500, "Failed to delete virtual key")
		return
	}
	// Removing key from in-memory store
	err = h.governanceManager.RemoveVirtualKey(ctx, vk.ID)
	if err != nil {
		// But we ignore this error because its not
		logger.Error("failed to remove virtual key: %v", err)
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Virtual key deleted successfully",
	})
}

// Team CRUD Operations

// getTeams handles GET /api/governance/teams - Get all teams
func (h *GovernanceHandler) getTeams(ctx *fasthttp.RequestCtx) {
	customerID := string(ctx.QueryArgs().Peek("customer_id"))
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		SendError(ctx, 410, "teams are deprecated; use organizations and governed_organization_id on budgets and rate limits")
		return
	}

	// Check for pagination parameters
	limitStr := string(ctx.QueryArgs().Peek("limit"))
	offsetStr := string(ctx.QueryArgs().Peek("offset"))
	search := string(ctx.QueryArgs().Peek("search"))

	if limitStr != "" || offsetStr != "" || search != "" {
		limit, _ := strconv.Atoi(limitStr)
		offset, _ := strconv.Atoi(offsetStr)
		limit, offset = ClampPaginationParams(limit, offset)
		teams, totalCount, err := h.cfg.StoreFromRequestCtx(ctx).GetTeamsPaginated(ctx, configstore.TeamsQueryParams{
			Limit:      limit,
			Offset:     offset,
			Search:     search,
			CustomerID: customerID,
		})
		if err != nil {
			logger.Error("failed to retrieve teams: %v", err)
			SendError(ctx, 500, fmt.Sprintf("Failed to retrieve teams: %v", err))
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"teams":       teams,
			"count":       len(teams),
			"total_count": totalCount,
			"limit":       limit,
			"offset":      offset,
		})
		return
	}

	// Non-paginated path: return all teams
	teams, err := h.cfg.StoreFromRequestCtx(ctx).GetTeams(ctx, customerID)
	if err != nil {
		logger.Error("failed to retrieve teams: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to retrieve teams: %v", err))
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"teams":       teams,
		"count":       len(teams),
		"total_count": len(teams),
		"limit":       len(teams),
		"offset":      0,
	})
}

// createTeam handles POST /api/governance/teams - Create a new team
func (h *GovernanceHandler) createTeam(ctx *fasthttp.RequestCtx) {
	var req CreateTeamRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Validate required fields
	if req.Name == "" {
		SendError(ctx, 400, "Team name is required")
		return
	}
	if len(req.Budgets) > 0 {
		SendError(ctx, 400, "team budgets are deprecated; attach budgets via governed_organization_id on budgets and rate limits")
		return
	}
	// Validate rate limit if provided
	if req.RateLimit != nil {
		rateLimit := configstoreTables.TableRateLimit{
			TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
			TokenResetDuration:   req.RateLimit.TokenResetDuration,
			RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
			RequestResetDuration: req.RateLimit.RequestResetDuration,
		}
		if err := validateRateLimit(&rateLimit); err != nil {
			SendError(ctx, 400, fmt.Sprintf("Invalid rate limit: %s", err.Error()))
			return
		}
	}
	// Creating team in database
	var team configstoreTables.TableTeam
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		team = configstoreTables.TableTeam{
			ID:              uuid.NewString(),
			Name:            req.Name,
			CustomerID:      req.CustomerID,
			CalendarAligned: req.CalendarAligned,
		}
	if req.RateLimit != nil {
			rateLimit := configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				SoftLimit:            req.RateLimit.SoftLimit,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).CreateRateLimit(ctx, &rateLimit, tx); err != nil {
				return err
			}
			team.RateLimitID = &rateLimit.ID
			team.RateLimit = &rateLimit
		}
		// Team row must exist before child budgets (FK on governance_budgets.team_id)
		if err := h.cfg.StoreFromRequestCtx(ctx).CreateTeam(ctx, &team, tx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		var badReqErr *badRequestError
		if errors.As(err, &badReqErr) {
			SendError(ctx, 400, err.Error())
			return
		}
		logger.Error("failed to create team: %v", err)
		SendError(ctx, 500, "failed to create team")
		return
	}
	// Reloading team from in-memory store
	preloadedTeam, err := h.governanceManager.ReloadTeam(ctx, team.ID)
	if err != nil {
		logger.Error("failed to reload team: %v", err)
		preloadedTeam = &team
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Team created successfully",
		"team":    preloadedTeam,
	})
}

// getTeam handles GET /api/governance/teams/{team_id} - Get a specific team
func (h *GovernanceHandler) getTeam(ctx *fasthttp.RequestCtx) {
	teamID := ctx.UserValue("team_id").(string)
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		SendError(ctx, 410, "teams are deprecated; use organizations and governed_organization_id on budgets and rate limits")
		return
	}
	team, err := h.cfg.StoreFromRequestCtx(ctx).GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Team not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve team")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"team": team,
	})
}

// updateTeam handles PUT /api/governance/teams/{team_id} - Update a team
func (h *GovernanceHandler) updateTeam(ctx *fasthttp.RequestCtx) {
	teamID := ctx.UserValue("team_id").(string)

	var req UpdateTeamRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Fetching team from database
	team, err := h.cfg.StoreFromRequestCtx(ctx).GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Team not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve team")
		return
	}
	// Updating team in database
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Track rate-limit ID to delete after updating the team (to avoid FK constraint)
		var rateLimitIDToDelete string

		// Update fields if provided
		if req.Name != nil {
			team.Name = *req.Name
		}
		if req.CustomerID != nil {
			if *req.CustomerID == "" {
				team.CustomerID = nil
			} else {
				team.CustomerID = req.CustomerID
			}
		}
		// Resolve team-level calendar alignment for this update:
		//   - explicit team-level field wins (req.CalendarAligned != nil)
		//   - else leave existing team.CalendarAligned untouched
		wasCalendarAligned := team.CalendarAligned
		if req.CalendarAligned != nil {
			team.CalendarAligned = *req.CalendarAligned
		}
		calendarAlignmentJustEnabled := !wasCalendarAligned && team.CalendarAligned
		// Snap-to-calendar-period happens after budget/rate-limit reconciliation
		// below, so combined `calendar_aligned + budgets/rate_limit` updates see
		// the final persisted state.

		// Multi-budget reconciliation is deprecated — use governed_organization_id on budgets and rate limits.
		if req.Budgets != nil {
			return &badRequestError{err: fmt.Errorf("team budgets are deprecated; attach budgets via governed_organization_id on budgets and rate limits")}
		}
		// Handle rate limit updates
		if req.RateLimit != nil {
			// Check if rate limit values are empty - means remove rate limit (reset durations don't matter)
			rateLimitIsEmpty := req.RateLimit.TokenMaxLimit == nil && req.RateLimit.RequestMaxLimit == nil
			if rateLimitIsEmpty {
				// Mark rate limit for deletion after FK is removed
				if team.RateLimitID != nil {
					rateLimitIDToDelete = *team.RateLimitID
					team.RateLimitID = nil
					team.RateLimit = nil
				}
			} else if team.RateLimitID != nil {
				// Update existing rate limit
				rateLimit := configstoreTables.TableRateLimit{}
				if err := tx.First(&rateLimit, "id = ?", *team.RateLimitID).Error; err != nil {
					return err
				}
		rateLimit.TokenMaxLimit = req.RateLimit.TokenMaxLimit
			rateLimit.TokenResetDuration = req.RateLimit.TokenResetDuration
			rateLimit.RequestMaxLimit = req.RateLimit.RequestMaxLimit
			rateLimit.RequestResetDuration = req.RateLimit.RequestResetDuration
			if req.RateLimit.SoftLimit != nil {
				rateLimit.SoftLimit = req.RateLimit.SoftLimit
			}
			if err := validateRateLimit(&rateLimit); err != nil {
				return err
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).UpdateRateLimit(ctx, &rateLimit, tx); err != nil {
				return err
			}
			team.RateLimit = &rateLimit
		} else {
			// Create new rate limit
			rateLimit := configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				SoftLimit:            req.RateLimit.SoftLimit,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
				if err := validateRateLimit(&rateLimit); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateRateLimit(ctx, &rateLimit, tx); err != nil {
					return err
				}
				team.RateLimitID = &rateLimit.ID
				team.RateLimit = &rateLimit
			}
		}
		// Snap budgets and rate limit to the current calendar period when calendar
		// alignment transitions false -> true in this request. Runs after budget/
		// rate-limit reconciliation so both the standalone-toggle and the combined
		// (toggle + budgets/rate_limit in the same request) cases are covered, and
		// only fires once per transition.
		if calendarAlignmentJustEnabled {
			now := time.Now()
			for i := range team.Budgets {
				b := &team.Budgets[i]
				if !configstoreTables.IsCalendarAlignableDuration(b.ResetDuration) {
					continue
				}
				b.LastReset = configstoreTables.GetCalendarPeriodStart(b.ResetDuration, now)
				b.CurrentUsage = 0
				if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, b, tx); err != nil {
					return fmt.Errorf("failed to snap team budget %s on calendar-align enable: %w", b.ID, err)
				}
			}
			if team.RateLimit != nil {
				rl := team.RateLimit
				snapped := false
				if rl.TokenResetDuration != nil && configstoreTables.IsCalendarAlignableDuration(*rl.TokenResetDuration) {
					rl.TokenLastReset = configstoreTables.GetCalendarPeriodStart(*rl.TokenResetDuration, now)
					rl.TokenCurrentUsage = 0
					snapped = true
				}
				if rl.RequestResetDuration != nil && configstoreTables.IsCalendarAlignableDuration(*rl.RequestResetDuration) {
					rl.RequestLastReset = configstoreTables.GetCalendarPeriodStart(*rl.RequestResetDuration, now)
					rl.RequestCurrentUsage = 0
					snapped = true
				}
				if snapped {
					if err := h.cfg.StoreFromRequestCtx(ctx).UpdateRateLimit(ctx, rl, tx); err != nil {
						return fmt.Errorf("failed to snap team rate limit on calendar-align enable: %w", err)
					}
				}
			}
		}
		if err := h.cfg.StoreFromRequestCtx(ctx).UpdateTeam(ctx, team, tx); err != nil {
			return err
		}

		// Now that FK references are removed, delete the orphaned rate limit.
		// Budgets are reconciled above (deletion of unmatched rows happens inside
		// the reconciliation loop), so nothing to clean up here.
		if rateLimitIDToDelete != "" {
			if err := tx.Delete(&configstoreTables.TableRateLimit{}, "id = ?", rateLimitIDToDelete).Error; err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		var badReqErr *badRequestError
		if errors.As(err, &badReqErr) {
			SendError(ctx, 400, err.Error())
			return
		}
		logger.Error("failed to update team: %v", err)
		SendError(ctx, 500, "Failed to update team")
		return
	}
	// Reloading team from in-memory store
	preloadedTeam, err := h.governanceManager.ReloadTeam(ctx, team.ID)
	if err != nil {
		logger.Error("failed to reload team: %v", err)
		preloadedTeam = team
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Team updated successfully",
		"team":    preloadedTeam,
	})
}

// deleteTeam handles DELETE /api/governance/teams/{team_id} - Delete a team
func (h *GovernanceHandler) deleteTeam(ctx *fasthttp.RequestCtx) {
	teamID := ctx.UserValue("team_id").(string)
	team, err := h.cfg.StoreFromRequestCtx(ctx).GetTeam(ctx, teamID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Team not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve team")
		return
	}
	// Removing team from in-memory store
	err = h.governanceManager.RemoveTeam(ctx, team.ID)
	if err != nil {
		// But we ignore this error because its not
		logger.Error("failed to remove team: %v", err)
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).DeleteTeam(ctx, teamID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Team not found")
			return
		}
		SendError(ctx, 500, "Failed to delete team")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Team deleted successfully",
	})
}

// Customer CRUD Operations

// getCustomers handles GET /api/governance/customers - Get all customers
func (h *GovernanceHandler) getCustomers(ctx *fasthttp.RequestCtx) {
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		SendError(ctx, 410, "customers are deprecated; use organizations and governed_organization_id on budgets and rate limits")
		return
	}
	limitStr := string(ctx.QueryArgs().Peek("limit"))
	offsetStr := string(ctx.QueryArgs().Peek("offset"))
	search := string(ctx.QueryArgs().Peek("search"))

	if limitStr != "" || offsetStr != "" || search != "" {
		limit, _ := strconv.Atoi(limitStr)
		offset, _ := strconv.Atoi(offsetStr)
		limit, offset = ClampPaginationParams(limit, offset)
		customers, totalCount, err := h.cfg.StoreFromRequestCtx(ctx).GetCustomersPaginated(ctx, configstore.CustomersQueryParams{
			Limit:  limit,
			Offset: offset,
			Search: search,
		})
		if err != nil {
			logger.Error("failed to retrieve customers: %v", err)
			SendError(ctx, 500, "failed to retrieve customers")
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"customers":   customers,
			"count":       len(customers),
			"total_count": totalCount,
			"limit":       limit,
			"offset":      offset,
		})
		return
	}

	customers, err := h.cfg.StoreFromRequestCtx(ctx).GetCustomers(ctx)
	if err != nil {
		logger.Error("failed to retrieve customers: %v", err)
		SendError(ctx, 500, "failed to retrieve customers")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"customers":   customers,
		"count":       len(customers),
		"total_count": len(customers),
		"limit":       len(customers),
		"offset":      0,
	})
}

// createCustomer handles POST /api/governance/customers - Create a new customer
func (h *GovernanceHandler) createCustomer(ctx *fasthttp.RequestCtx) {
	var req CreateCustomerRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Validate required fields
	if req.Name == "" {
		SendError(ctx, 400, "Customer name is required")
		return
	}
	// Validate rate limit if provided
	if req.RateLimit != nil {
		rateLimit := configstoreTables.TableRateLimit{
			TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
			TokenResetDuration:   req.RateLimit.TokenResetDuration,
			RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
			RequestResetDuration: req.RateLimit.RequestResetDuration,
		}
		if err := validateRateLimit(&rateLimit); err != nil {
			SendError(ctx, 400, fmt.Sprintf("Invalid rate limit: %s", err.Error()))
			return
		}
	}
	var customer configstoreTables.TableCustomer
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		customer = configstoreTables.TableCustomer{
			ID:   uuid.NewString(),
			Name: req.Name,
		}

	if req.Budget != nil {
			budget := configstoreTables.TableBudget{
				ID:            uuid.NewString(),
				MaxLimit:      req.Budget.MaxLimit,
				ResetDuration: req.Budget.ResetDuration,
				SoftLimit:     req.Budget.SoftLimit,
				LastReset:     budgetLastReset(false, req.Budget.ResetDuration),
				CurrentUsage:  0,
			}
			if err := validateBudget(&budget); err != nil {
				return err
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
				return err
			}
			customer.BudgetID = &budget.ID
		}
		if req.RateLimit != nil {
			rateLimit := configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				SoftLimit:            req.RateLimit.SoftLimit,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).CreateRateLimit(ctx, &rateLimit, tx); err != nil {
				return err
			}
			customer.RateLimitID = &rateLimit.ID
		}
		if err := h.cfg.StoreFromRequestCtx(ctx).CreateCustomer(ctx, &customer, tx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		SendError(ctx, 500, "failed to create customer")
		return
	}
	preloadedCustomer, err := h.governanceManager.ReloadCustomer(ctx, customer.ID)
	if err != nil {
		logger.Error("failed to reload customer: %v", err)
		preloadedCustomer = &customer
	}
	SendJSON(ctx, map[string]interface{}{
		"message":  "Customer created successfully",
		"customer": preloadedCustomer,
	})
}

// getCustomer handles GET /api/governance/customers/{customer_id} - Get a specific customer
func (h *GovernanceHandler) getCustomer(ctx *fasthttp.RequestCtx) {
	customerID := ctx.UserValue("customer_id").(string)
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		SendError(ctx, 410, "customers are deprecated; use organizations and governed_organization_id on budgets and rate limits")
		return
	}
	customer, err := h.cfg.StoreFromRequestCtx(ctx).GetCustomer(ctx, customerID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Customer not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve customer")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"customer": customer,
	})
}

// updateCustomer handles PUT /api/governance/customers/{customer_id} - Update a customer
func (h *GovernanceHandler) updateCustomer(ctx *fasthttp.RequestCtx) {
	customerID := ctx.UserValue("customer_id").(string)
	var req UpdateCustomerRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Fetching customer from database
	customer, err := h.cfg.StoreFromRequestCtx(ctx).GetCustomer(ctx, customerID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Customer not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve customer")
		return
	}
	// Updating customer in database
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Track IDs to delete after updating the customer (to avoid FK constraint)
		var budgetIDToDelete, rateLimitIDToDelete string

		// Update fields if provided
		if req.Name != nil {
			customer.Name = *req.Name
		}
		// Handle budget updates
		if req.Budget != nil {
			// Check if budget removal is requested (all fields nil)
			budgetIsEmpty := isBudgetRemovalRequest(req.Budget)
			if budgetIsEmpty {
				// Mark budget for deletion after FK is removed
				if customer.BudgetID != nil {
					budgetIDToDelete = *customer.BudgetID
					customer.BudgetID = nil
					customer.Budget = nil
				}
			} else if customer.BudgetID != nil {
				// Update existing budget — all fields are optional (partial update)
				budget := configstoreTables.TableBudget{}
				if err := tx.First(&budget, "id = ?", *customer.BudgetID).Error; err != nil {
					return err
				}
			if req.Budget.MaxLimit != nil {
				budget.MaxLimit = *req.Budget.MaxLimit
			}
			if req.Budget.ResetDuration != nil {
				budget.ResetDuration = *req.Budget.ResetDuration
			}
			if req.Budget.SoftLimit != nil {
				budget.SoftLimit = req.Budget.SoftLimit
			}
			if err := validateBudget(&budget); err != nil {
				return err
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, &budget, tx); err != nil {
				return err
			}
			customer.Budget = &budget
		} else {
			// Create new budget
			if req.Budget.MaxLimit == nil || req.Budget.ResetDuration == nil {
				return fmt.Errorf("both max_limit and reset_duration are required when creating a new budget")
			}
			if *req.Budget.MaxLimit < 0 {
				return fmt.Errorf("budget max_limit cannot be negative: %.2f", *req.Budget.MaxLimit)
			}
			if _, err := configstoreTables.ParseDuration(*req.Budget.ResetDuration); err != nil {
				return fmt.Errorf("invalid reset duration format: %s", *req.Budget.ResetDuration)
			}
			budget := configstoreTables.TableBudget{
				ID:            uuid.NewString(),
				MaxLimit:      *req.Budget.MaxLimit,
				ResetDuration: *req.Budget.ResetDuration,
				SoftLimit:     req.Budget.SoftLimit,
				LastReset:     budgetLastReset(false, *req.Budget.ResetDuration),
				CurrentUsage:  0,
			}
				if err := validateBudget(&budget); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
					return err
				}
				customer.BudgetID = &budget.ID
				customer.Budget = &budget
			}
		}
		// Handle rate limit updates
		if req.RateLimit != nil {
			// Check if rate limit values are empty - means remove rate limit (reset durations don't matter)
			rateLimitIsEmpty := req.RateLimit.TokenMaxLimit == nil && req.RateLimit.RequestMaxLimit == nil
			if rateLimitIsEmpty {
				// Mark rate limit for deletion after FK is removed
				if customer.RateLimitID != nil {
					rateLimitIDToDelete = *customer.RateLimitID
					customer.RateLimitID = nil
					customer.RateLimit = nil
				}
			} else if customer.RateLimitID != nil {
				// Update existing rate limit
				rateLimit := configstoreTables.TableRateLimit{}
				if err := tx.First(&rateLimit, "id = ?", *customer.RateLimitID).Error; err != nil {
					return err
				}
			rateLimit.TokenMaxLimit = req.RateLimit.TokenMaxLimit
			rateLimit.TokenResetDuration = req.RateLimit.TokenResetDuration
			rateLimit.RequestMaxLimit = req.RateLimit.RequestMaxLimit
			rateLimit.RequestResetDuration = req.RateLimit.RequestResetDuration
			if req.RateLimit.SoftLimit != nil {
				rateLimit.SoftLimit = req.RateLimit.SoftLimit
			}
			if err := validateRateLimit(&rateLimit); err != nil {
				return err
			}
			if err := h.cfg.StoreFromRequestCtx(ctx).UpdateRateLimit(ctx, &rateLimit, tx); err != nil {
				return err
			}
			customer.RateLimit = &rateLimit
		} else {
			// Create new rate limit
			rateLimit := configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				SoftLimit:            req.RateLimit.SoftLimit,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
				if err := validateRateLimit(&rateLimit); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateRateLimit(ctx, &rateLimit, tx); err != nil {
					return err
				}
				customer.RateLimitID = &rateLimit.ID
				customer.RateLimit = &rateLimit
			}
		}
		if err := h.cfg.StoreFromRequestCtx(ctx).UpdateCustomer(ctx, customer, tx); err != nil {
			return err
		}

		// Now that FK references are removed, delete the orphaned budget/rate limit
		if budgetIDToDelete != "" {
			if err := tx.Delete(&configstoreTables.TableBudget{}, "id = ?", budgetIDToDelete).Error; err != nil {
				return err
			}
		}
		if rateLimitIDToDelete != "" {
			if err := tx.Delete(&configstoreTables.TableRateLimit{}, "id = ?", rateLimitIDToDelete).Error; err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		SendError(ctx, 500, "Failed to update customer")
		return
	}

	preloadedCustomer, err := h.governanceManager.ReloadCustomer(ctx, customer.ID)
	if err != nil {
		logger.Error("failed to reload customer: %v", err)
		preloadedCustomer = customer
	}

	SendJSON(ctx, map[string]interface{}{
		"message":  "Customer updated successfully",
		"customer": preloadedCustomer,
	})
}

// deleteCustomer handles DELETE /api/governance/customers/{customer_id} - Delete a customer
func (h *GovernanceHandler) deleteCustomer(ctx *fasthttp.RequestCtx) {
	customerID := ctx.UserValue("customer_id").(string)

	customer, err := h.cfg.StoreFromRequestCtx(ctx).GetCustomer(ctx, customerID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Customer not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve customer")
		return
	}
	err = h.governanceManager.RemoveCustomer(ctx, customer.ID)
	if err != nil {
		// But we ignore this error because its not
		logger.Error("failed to remove customer: %v", err)
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).DeleteCustomer(ctx, customerID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Customer not found")
			return
		}
		SendError(ctx, 500, "Failed to delete customer")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Customer deleted successfully",
	})
}

// Budget and Rate Limit GET operations

// getBudgets handles GET /api/governance/budgets - Get all budgets
func (h *GovernanceHandler) getBudgets(ctx *fasthttp.RequestCtx) {
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"budgets": data.Budgets,
			"count":   len(data.Budgets),
		})
		return
	}
	budgets, err := h.cfg.StoreFromRequestCtx(ctx).GetBudgets(ctx)
	if err != nil {
		logger.Error("failed to retrieve budgets: %v", err)
		SendError(ctx, 500, "failed to retrieve budgets")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"budgets": budgets,
		"count":   len(budgets),
	})
}

// getRateLimits handles GET /api/governance/rate-limits - Get all rate limits
func (h *GovernanceHandler) getRateLimits(ctx *fasthttp.RequestCtx) {
	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"rate_limits": data.RateLimits,
			"count":       len(data.RateLimits),
		})
		return
	}
	rateLimits, err := h.cfg.StoreFromRequestCtx(ctx).GetRateLimits(ctx)
	if err != nil {
		logger.Error("failed to retrieve rate limits: %v", err)
		SendError(ctx, 500, "failed to retrieve rate limits")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"rate_limits": rateLimits,
		"count":       len(rateLimits),
	})
}

// validateRateLimit validates the rate limit
func validateRateLimit(rateLimit *configstoreTables.TableRateLimit) error {
	if rateLimit.TokenMaxLimit != nil && (*rateLimit.TokenMaxLimit < 0 || *rateLimit.TokenMaxLimit == 0) {
		return fmt.Errorf("rate limit token max limit cannot be negative or zero: %d", *rateLimit.TokenMaxLimit)
	}
	// Only require token reset duration if token limit is set
	if rateLimit.TokenMaxLimit != nil {
		if rateLimit.TokenResetDuration == nil {
			return fmt.Errorf("rate limit token reset duration is required")
		}
		if _, err := configstoreTables.ParseDuration(*rateLimit.TokenResetDuration); err != nil {
			return fmt.Errorf("invalid rate limit token reset duration format: %s", *rateLimit.TokenResetDuration)
		}
	}
	if rateLimit.RequestMaxLimit != nil && (*rateLimit.RequestMaxLimit < 0 || *rateLimit.RequestMaxLimit == 0) {
		return fmt.Errorf("rate limit request max limit cannot be negative or zero: %d", *rateLimit.RequestMaxLimit)
	}
	// Only require request reset duration if request limit is set
	if rateLimit.RequestMaxLimit != nil {
		if rateLimit.RequestResetDuration == nil {
			return fmt.Errorf("rate limit request reset duration is required")
		}
		if _, err := configstoreTables.ParseDuration(*rateLimit.RequestResetDuration); err != nil {
			return fmt.Errorf("invalid rate limit request reset duration format: %s", *rateLimit.RequestResetDuration)
		}
	}
	return nil
}

func (h *GovernanceHandler) getConfiguredProviderSet(ctx context.Context) (map[schemas.ModelProvider]struct{}, error) {
	providers, err := h.cfg.StoreFromContext(ctx).GetProviders(ctx)
	if err != nil {
		return nil, err
	}
	providerSet := make(map[schemas.ModelProvider]struct{}, len(providers))
	for _, provider := range providers {
		providerName := schemas.ModelProvider(strings.TrimSpace(provider.Name))
		if providerName == "" {
			continue
		}
		providerSet[providerName] = struct{}{}
	}
	return providerSet, nil
}

// validateBudget validates the budget
func validateBudget(budget *configstoreTables.TableBudget) error {
	if budget.MaxLimit < 0 || budget.MaxLimit == 0 {
		return fmt.Errorf("budget max limit cannot be negative or zero: %.2f", budget.MaxLimit)
	}
	if budget.ResetDuration == "" {
		return fmt.Errorf("budget reset duration is required")
	}
	if _, err := configstoreTables.ParseDuration(budget.ResetDuration); err != nil {
		return fmt.Errorf("invalid budget reset duration format: %s", budget.ResetDuration)
	}
	return nil
}

// Model Config CRUD Operations

// getModelConfigs handles GET /api/governance/model-configs - Get all model configs
func (h *GovernanceHandler) getModelConfigs(ctx *fasthttp.RequestCtx) {
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		SendJSON(ctx, map[string]any{
			"model_configs": data.ModelConfigs,
			"count":         len(data.ModelConfigs),
			"total_count":   len(data.ModelConfigs),
			"limit":         len(data.ModelConfigs),
			"offset":        0,
		})
		return
	}

	// Check for pagination parameters
	limitStr := string(ctx.QueryArgs().Peek("limit"))
	offsetStr := string(ctx.QueryArgs().Peek("offset"))
	search := string(ctx.QueryArgs().Peek("search"))

	if limitStr != "" || offsetStr != "" || search != "" {
		// Paginated path
		params := configstore.ModelConfigsQueryParams{
			Search: search,
		}
		if limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil {
				SendError(ctx, 400, "Invalid limit parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid limit parameter: must be non-negative")
				return
			}
			params.Limit = n
		}
		if offsetStr != "" {
			n, err := strconv.Atoi(offsetStr)
			if err != nil {
				SendError(ctx, 400, "Invalid offset parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid offset parameter: must be non-negative")
				return
			}
			params.Offset = n
		}

		params.Limit, params.Offset = ClampPaginationParams(params.Limit, params.Offset)
		modelConfigs, totalCount, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfigsPaginated(ctx, params)
		if err != nil {
			logger.Error("failed to retrieve model configs: %v", err)
			SendError(ctx, 500, "Failed to retrieve model configs")
			return
		}
		SendJSON(ctx, map[string]any{
			"model_configs": modelConfigs,
			"count":         len(modelConfigs),
			"total_count":   totalCount,
			"limit":         params.Limit,
			"offset":        params.Offset,
		})
		return
	}

	// Non-paginated path: return all model configs
	modelConfigs, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfigs(ctx)
	if err != nil {
		logger.Error("failed to retrieve model configs: %v", err)
		SendError(ctx, 500, "Failed to retrieve model configs")
		return
	}
	SendJSON(ctx, map[string]any{
		"model_configs": modelConfigs,
		"count":         len(modelConfigs),
		"total_count":   len(modelConfigs),
		"limit":         len(modelConfigs),
		"offset":        0,
	})
}

// getModelConfig handles GET /api/governance/model-configs/{mc_id} - Get a specific model config
func (h *GovernanceHandler) getModelConfig(ctx *fasthttp.RequestCtx) {
	mcID := ctx.UserValue("mc_id").(string)
	mc, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfigByID(ctx, mcID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Model config not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve model config")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"model_config": mc,
	})
}

// createModelConfig handles POST /api/governance/model-configs - Create a new model config
func (h *GovernanceHandler) createModelConfig(ctx *fasthttp.RequestCtx) {
	var req CreateModelConfigRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Validate required fields
	if req.ModelName == "" {
		SendError(ctx, 400, "Model name is required")
		return
	}
	// Check if model config with same (model_name, provider) already exists
	existing, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfig(ctx, req.ModelName, req.Provider)
	if err != nil && err != configstore.ErrNotFound {
		logger.Error("failed to check existing model config: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to check existing model config: %v", err))
		return
	}
	if existing != nil {
		if req.Provider != nil {
			SendError(ctx, 409, fmt.Sprintf("Model config for model '%s' with provider '%s' already exists", req.ModelName, *req.Provider))
		} else {
			SendError(ctx, 409, fmt.Sprintf("Model config for model '%s' (global) already exists", req.ModelName))
		}
		return
	}
	if len(req.Budgets) > 0 {
		seenDurations := make(map[string]bool)
		for _, b := range req.Budgets {
			if b.MaxLimit < 0 {
				SendError(ctx, 400, fmt.Sprintf("Budget max_limit cannot be negative: %.2f", b.MaxLimit))
				return
			}
			if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
				SendError(ctx, 400, fmt.Sprintf("Invalid reset duration format: %s", b.ResetDuration))
				return
			}
			if seenDurations[b.ResetDuration] {
				SendError(ctx, 400, fmt.Sprintf("Duplicate reset_duration in budgets: %s", b.ResetDuration))
				return
			}
			seenDurations[b.ResetDuration] = true
		}
	}
	var mc configstoreTables.TableModelConfig
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		mc = configstoreTables.TableModelConfig{
			ID:        uuid.NewString(),
			ModelName: req.ModelName,
			Provider:  req.Provider,
			SystemColumns: configstoreTables.SystemColumns{
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
		}
		if err := h.cfg.StoreFromRequestCtx(ctx).CreateModelConfig(ctx, &mc, tx); err != nil {
			return err
		}
		mcID := mc.ID
	if len(req.Budgets) > 0 {
			for _, b := range req.Budgets {
				budget := configstoreTables.TableBudget{
					ID:            uuid.NewString(),
					MaxLimit:      b.MaxLimit,
					ResetDuration: b.ResetDuration,
					SoftLimit:     b.SoftLimit,
					LastReset:     budgetLastReset(false, b.ResetDuration),
					CurrentUsage:  0,
					ModelConfigID: &mcID,
				}
				if err := validateBudget(&budget); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
					return err
				}
				mc.Budgets = append(mc.Budgets, budget)
			}
		}
		if len(req.RateLimits) > 0 {
			reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, nil, req.RateLimits, func(rl *configstoreTables.TableRateLimit) {
				rl.ModelConfigID = &mcID
			})
			if err != nil {
				return err
			}
			mc.RateLimits = reconciled
		}
		return nil
	}); err != nil {
		logger.Error("failed to create model config: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to create model config: %v", err))
		return
	}
	// Reload model config in memory
	preloadedMC, err := h.governanceManager.ReloadModelConfig(ctx, mc.ID)
	if err != nil {
		logger.Error("failed to reload model config in memory: %v", err)
		preloadedMC = &mc
	}
	SendJSON(ctx, map[string]interface{}{
		"message":      "Model config created successfully",
		"model_config": preloadedMC,
	})
}

// updateModelConfig handles PUT /api/governance/model-configs/{mc_id} - Update a model config
func (h *GovernanceHandler) updateModelConfig(ctx *fasthttp.RequestCtx) {
	mcID := ctx.UserValue("mc_id").(string)
	var req UpdateModelConfigRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	mc, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfigByID(ctx, mcID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Model config not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve model config")
		return
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		if req.ModelName != nil {
			mc.ModelName = *req.ModelName
		}
		if req.Provider != nil {
			mc.Provider = req.Provider
		}
		if req.Budgets != nil {
			requestBudgets := append([]CreateBudgetRequest(nil), req.Budgets...)
			sort.Slice(requestBudgets, func(i, j int) bool {
				return compareBudgetRequestDurations(requestBudgets[i], requestBudgets[j])
			})
			seenDurations := make(map[string]bool)
			for _, b := range requestBudgets {
				if b.MaxLimit < 0 {
					return fmt.Errorf("budget max_limit cannot be negative: %.2f", b.MaxLimit)
				}
				if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
					return fmt.Errorf("invalid reset duration format: %s", b.ResetDuration)
				}
				if seenDurations[b.ResetDuration] {
					return fmt.Errorf("duplicate reset_duration in budgets: %s", b.ResetDuration)
				}
				seenDurations[b.ResetDuration] = true
			}

			sort.Slice(mc.Budgets, func(i, j int) bool {
				if mc.Budgets[i].ResetDuration == mc.Budgets[j].ResetDuration {
					return mc.Budgets[i].ID < mc.Budgets[j].ID
				}
				return mc.Budgets[i].ResetDuration < mc.Budgets[j].ResetDuration
			})

			existingByID, existingByDuration := buildBudgetLookup(mc.Budgets, requestBudgets)
			var reconciledBudgets []configstoreTables.TableBudget
			matchedIDs := make(map[string]bool)
			for _, b := range requestBudgets {
				existing, found, err := findExistingBudget(b, existingByID, existingByDuration)
				if err != nil {
					return err
				}
			if found {
				existing.MaxLimit = b.MaxLimit
				existing.ResetDuration = b.ResetDuration
				if b.SoftLimit != nil {
					existing.SoftLimit = b.SoftLimit
				}
				if err := validateBudget(&existing); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, &existing, tx); err != nil {
					return err
				}
				reconciledBudgets = append(reconciledBudgets, existing)
				matchedIDs[existing.ID] = true
			} else {
				budget := configstoreTables.TableBudget{
					ID:            uuid.NewString(),
					MaxLimit:      b.MaxLimit,
					ResetDuration: b.ResetDuration,
					SoftLimit:     b.SoftLimit,
					LastReset:     budgetLastReset(false, b.ResetDuration),
					CurrentUsage:  0,
					ModelConfigID: &mc.ID,
				}
					inheritUsageFromClosestShorterBudget(&budget, mc.Budgets, false)
					if err := validateBudget(&budget); err != nil {
						return err
					}
					if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
						return err
					}
					reconciledBudgets = append(reconciledBudgets, budget)
				}
			}
			for _, existing := range mc.Budgets {
				if !matchedIDs[existing.ID] {
					if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, existing.ID, tx); err != nil {
						return fmt.Errorf("failed to delete removed model config budget: %w", err)
					}
				}
			}
			mc.Budgets = reconciledBudgets
		}
		if req.RateLimits != nil {
			mcID := mc.ID
			reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, mc.RateLimits, req.RateLimits, func(rl *configstoreTables.TableRateLimit) {
				rl.ModelConfigID = &mcID
			})
			if err != nil {
				return err
			}
			mc.RateLimits = reconciled
		}
		mc.UpdatedAt = time.Now()
		if err := h.cfg.StoreFromRequestCtx(ctx).UpdateModelConfig(ctx, mc, tx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		logger.Error("failed to update model config: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to update model config: %v", err))
		return
	}
	// Reload model config in memory (also reloads from DB to get full relationships)
	updatedMC, err := h.governanceManager.ReloadModelConfig(ctx, mc.ID)
	if err != nil {
		logger.Error("failed to reload model config in memory: %v", err)
		updatedMC = mc
	}
	SendJSON(ctx, map[string]interface{}{
		"message":      "Model config updated successfully",
		"model_config": updatedMC,
	})
}

// deleteModelConfig handles DELETE /api/governance/model-configs/{mc_id} - Delete a model config
func (h *GovernanceHandler) deleteModelConfig(ctx *fasthttp.RequestCtx) {
	mcID := ctx.UserValue("mc_id").(string)
	// Check if model config exists
	_, err := h.cfg.StoreFromRequestCtx(ctx).GetModelConfigByID(ctx, mcID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Model config not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve model config")
		return
	}
	// Delete the model config
	if err := h.cfg.StoreFromRequestCtx(ctx).DeleteModelConfig(ctx, mcID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Model config not found")
			return
		}
		logger.Error("failed to delete model config: %v", err)
		SendError(ctx, 500, "Failed to delete model config")
		return
	}
	// Remove model config from in-memory store
	if err := h.governanceManager.RemoveModelConfig(ctx, mcID); err != nil {
		logger.Error("failed to remove model config from memory: %v", err)
		// Continue anyway, the config is deleted from DB
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Model config deleted successfully",
	})
}

// Provider Governance Operations

// ProviderGovernanceResponse represents a provider with its governance settings
type ProviderGovernanceResponse struct {
	Provider   string                             `json:"provider"`
	Budgets    []configstoreTables.TableBudget    `json:"budgets,omitempty"`
	RateLimits []configstoreTables.TableRateLimit `json:"rate_limits,omitempty"`
}

// getProviderGovernance handles GET /api/governance/providers - Get all providers with governance settings
func (h *GovernanceHandler) getProviderGovernance(ctx *fasthttp.RequestCtx) {
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		data := h.governanceManager.GetGovernanceData(ctx)
		if data == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		var result []ProviderGovernanceResponse
		for _, p := range data.Providers {
			if len(p.Budgets) > 0 || len(p.RateLimits) > 0 {
				result = append(result, ProviderGovernanceResponse{
					Provider:   p.Name,
					Budgets:    p.Budgets,
					RateLimits: p.RateLimits,
				})
			}
		}
		SendJSON(ctx, map[string]interface{}{
			"providers": result,
			"count":     len(result),
		})
		return
	}
	providers, err := h.cfg.StoreFromRequestCtx(ctx).GetProviders(ctx)
	if err != nil {
		logger.Error("failed to retrieve providers: %v", err)
		SendError(ctx, 500, "Failed to retrieve providers")
		return
	}
	// Transform to governance response format
	var result []ProviderGovernanceResponse
	for _, p := range providers {
		if len(p.Budgets) > 0 || len(p.RateLimits) > 0 {
			result = append(result, ProviderGovernanceResponse{
				Provider:   p.Name,
				Budgets:    p.Budgets,
				RateLimits: p.RateLimits,
			})
		}
	}
	SendJSON(ctx, map[string]interface{}{
		"providers": result,
		"count":     len(result),
	})
}

// updateProviderGovernance handles PUT /api/governance/providers/{provider_name} - Update provider governance
func (h *GovernanceHandler) updateProviderGovernance(ctx *fasthttp.RequestCtx) {
	providerName := ctx.UserValue("provider_name").(string)
	var req UpdateProviderGovernanceRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}
	// Get all providers and find the one we need
	providers, err := h.cfg.StoreFromRequestCtx(ctx).GetProviders(ctx)
	if err != nil {
		SendError(ctx, 500, "Failed to retrieve providers")
		return
	}
	var provider *configstoreTables.TableProvider
	for i := range providers {
		if providers[i].Name == providerName {
			provider = &providers[i]
			break
		}
	}
	if provider == nil {
		SendError(ctx, 404, "Provider not found")
		return
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		providerID := provider.ID
		if req.Budgets != nil {
			requestBudgets := append([]CreateBudgetRequest(nil), req.Budgets...)
			sort.Slice(requestBudgets, func(i, j int) bool {
				return compareBudgetRequestDurations(requestBudgets[i], requestBudgets[j])
			})
			seenDurations := make(map[string]bool)
			for _, b := range requestBudgets {
				if b.MaxLimit < 0 {
					return fmt.Errorf("budget max_limit cannot be negative: %.2f", b.MaxLimit)
				}
				if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
					return fmt.Errorf("invalid reset duration format: %s", b.ResetDuration)
				}
				if seenDurations[b.ResetDuration] {
					return fmt.Errorf("duplicate reset_duration in budgets: %s", b.ResetDuration)
				}
				seenDurations[b.ResetDuration] = true
			}

			sort.Slice(provider.Budgets, func(i, j int) bool {
				if provider.Budgets[i].ResetDuration == provider.Budgets[j].ResetDuration {
					return provider.Budgets[i].ID < provider.Budgets[j].ID
				}
				return provider.Budgets[i].ResetDuration < provider.Budgets[j].ResetDuration
			})

			existingByID, existingByDuration := buildBudgetLookup(provider.Budgets, requestBudgets)
			var reconciledBudgets []configstoreTables.TableBudget
			matchedIDs := make(map[string]bool)
			for _, b := range requestBudgets {
				existing, found, err := findExistingBudget(b, existingByID, existingByDuration)
				if err != nil {
					return err
				}
			if found {
				existing.MaxLimit = b.MaxLimit
				existing.ResetDuration = b.ResetDuration
				if b.SoftLimit != nil {
					existing.SoftLimit = b.SoftLimit
				}
				if err := validateBudget(&existing); err != nil {
					return err
				}
				if err := h.cfg.StoreFromRequestCtx(ctx).UpdateBudget(ctx, &existing, tx); err != nil {
					return err
				}
				reconciledBudgets = append(reconciledBudgets, existing)
				matchedIDs[existing.ID] = true
			} else {
				budget := configstoreTables.TableBudget{
					ID:            uuid.NewString(),
					MaxLimit:      b.MaxLimit,
					ResetDuration: b.ResetDuration,
					SoftLimit:     b.SoftLimit,
					LastReset:     budgetLastReset(false, b.ResetDuration),
					CurrentUsage:  0,
					ProviderID:    &providerID,
				}
					inheritUsageFromClosestShorterBudget(&budget, provider.Budgets, false)
					if err := validateBudget(&budget); err != nil {
						return err
					}
					if err := h.cfg.StoreFromRequestCtx(ctx).CreateBudget(ctx, &budget, tx); err != nil {
						return err
					}
					reconciledBudgets = append(reconciledBudgets, budget)
				}
			}
			for _, existing := range provider.Budgets {
				if !matchedIDs[existing.ID] {
					if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, existing.ID, tx); err != nil {
						return fmt.Errorf("failed to delete removed provider budget: %w", err)
					}
				}
			}
			provider.Budgets = reconciledBudgets
		}
		if req.RateLimits != nil {
			reconciled, err := reconcileRateLimitRequests(ctx, h.cfg.StoreFromRequestCtx(ctx), tx, provider.RateLimits, req.RateLimits, func(rl *configstoreTables.TableRateLimit) {
				rl.ProviderID = &providerID
			})
			if err != nil {
				return err
			}
			provider.RateLimits = reconciled
		}
		return nil
	}); err != nil {
		logger.Error("failed to update provider governance: %v", err)
		SendError(ctx, 500, fmt.Sprintf("Failed to update provider governance: %v", err))
		return
	}
	// Reload provider in memory
	updatedProvider, err := h.governanceManager.ReloadProvider(ctx, schemas.ModelProvider(providerName))
	if err != nil {
		logger.Error("failed to reload provider in memory: %v", err)
		// Use the local provider object if reload fails
	} else {
		provider = updatedProvider
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Provider governance updated successfully",
		"provider": ProviderGovernanceResponse{
			Provider:   provider.Name,
			Budgets:    provider.Budgets,
			RateLimits: provider.RateLimits,
		},
	})
}

// deleteProviderGovernance handles DELETE /api/governance/providers/{provider_name} - Remove governance from provider
func (h *GovernanceHandler) deleteProviderGovernance(ctx *fasthttp.RequestCtx) {
	providerName := ctx.UserValue("provider_name").(string)
	// Get all providers and find the one we need
	providers, err := h.cfg.StoreFromRequestCtx(ctx).GetProviders(ctx)
	if err != nil {
		SendError(ctx, 500, "Failed to retrieve providers")
		return
	}
	var provider *configstoreTables.TableProvider
	for i := range providers {
		if providers[i].Name == providerName {
			provider = &providers[i]
			break
		}
	}
	if provider == nil {
		SendError(ctx, 404, "Provider not found")
		return
	}
	if err := h.cfg.StoreFromRequestCtx(ctx).ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		for _, b := range provider.Budgets {
			if err := h.cfg.StoreFromRequestCtx(ctx).DeleteBudget(ctx, b.ID, tx); err != nil {
				return err
			}
		}
		provider.Budgets = nil
		for _, rl := range provider.RateLimits {
			if err := h.cfg.StoreFromRequestCtx(ctx).DeleteRateLimit(ctx, rl.ID, tx); err != nil {
				return err
			}
		}
		provider.RateLimits = nil
		return nil
	}); err != nil {
		logger.Error("failed to delete provider governance: %v", err)
		SendError(ctx, 500, "Failed to delete provider governance")
		return
	}
	// Reload provider in memory (to clear the budget/rate limit)
	if _, err := h.governanceManager.ReloadProvider(ctx, schemas.ModelProvider(providerName)); err != nil {
		logger.Error("failed to reload provider in memory: %v", err)
		// Continue anyway, the governance is deleted from DB
	}
	SendJSON(ctx, map[string]interface{}{
		"message": "Provider governance deleted successfully",
	})
}

// Routing Rules CRUD Operations

// getRoutingRules retrieves all routing rules with optional filtering from database
func (h *GovernanceHandler) getRoutingRules(ctx *fasthttp.RequestCtx) {
	// Get query parameters for filtering
	scope := string(ctx.QueryArgs().Peek("scope"))
	scopeID := string(ctx.QueryArgs().Peek("scope_id"))
	scopeOrgID := string(ctx.QueryArgs().Peek("scope_org_id"))
	virtualKeyID := string(ctx.QueryArgs().Peek("virtual_key_id"))

	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		gd := h.governanceManager.GetGovernanceData(ctx)
		if gd == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		inMemoryRules := gd.RoutingRules

		// Filter rules by scope and scopeID
		var rules []configstoreTables.TableRoutingRule
		for _, rule := range inMemoryRules {
			if scopeOrgID != "" {
				if rule.RoutingScopeOrgID() != scopeOrgID {
					continue
				}
			}
			if virtualKeyID != "" {
				if rule.VirtualKeyID == nil || *rule.VirtualKeyID != virtualKeyID {
					continue
				}
			}
			if scope != "" && scopeOrgID == "" && virtualKeyID == "" {
				if rule.RoutingScopeName() != scope {
					continue
				}
			}
			if scopeID != "" && scopeOrgID == "" && virtualKeyID == "" {
				ruleScopeID := rule.RoutingScopeOrgID()
				if rule.VirtualKeyID != nil {
					ruleScopeID = *rule.VirtualKeyID
				}
				if ruleScopeID != scopeID {
					continue
				}
			}
			rules = append(rules, *rule)
		}

		SendJSON(ctx, map[string]interface{}{
			"rules":       rules,
			"count":       len(rules),
			"total_count": len(rules),
			"limit":       len(rules),
			"offset":      0,
		})
		return
	}

	// If scope_org_id/virtual_key_id or legacy scope filters are specified, use scoped lookup.
	if scopeOrgID != "" {
		scope = "org"
		scopeID = scopeOrgID
	} else if virtualKeyID != "" {
		scope = "virtual_key"
		scopeID = virtualKeyID
	}
	if scope != "" || scopeID != "" {
		rules, err := h.cfg.StoreFromRequestCtx(ctx).GetRoutingRulesByScope(ctx, scope, scopeID)
		if err != nil {
			SendError(ctx, 500, "Failed to get routing rules")
			return
		}
		response := make([]configstoreTables.TableRoutingRule, 0, len(rules))
		for _, rule := range rules {
			response = append(response, rule)
		}
		SendJSON(ctx, map[string]interface{}{
			"rules":       response,
			"count":       len(response),
			"total_count": len(response),
			"limit":       len(response),
			"offset":      0,
		})
		return
	}

	// Check for pagination parameters
	limitStr := string(ctx.QueryArgs().Peek("limit"))
	offsetStr := string(ctx.QueryArgs().Peek("offset"))
	search := string(ctx.QueryArgs().Peek("search"))

	if limitStr != "" || offsetStr != "" || search != "" {
		// Paginated path
		params := configstore.RoutingRulesQueryParams{
			Search: search,
		}
		if limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil {
				SendError(ctx, 400, "Invalid limit parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid limit parameter: must be non-negative")
				return
			}
			params.Limit = n
		}
		if offsetStr != "" {
			n, err := strconv.Atoi(offsetStr)
			if err != nil {
				SendError(ctx, 400, "Invalid offset parameter: must be a number")
				return
			}
			if n < 0 {
				SendError(ctx, 400, "Invalid offset parameter: must be non-negative")
				return
			}
			params.Offset = n
		}

		params.Limit, params.Offset = ClampPaginationParams(params.Limit, params.Offset)
		rules, totalCount, err := h.cfg.StoreFromRequestCtx(ctx).GetRoutingRulesPaginated(ctx, params)
		if err != nil {
			logger.Error("failed to retrieve routing rules: %v", err)
			SendError(ctx, 500, "Failed to retrieve routing rules")
			return
		}
		SendJSON(ctx, map[string]interface{}{
			"rules":       rules,
			"count":       len(rules),
			"total_count": totalCount,
			"limit":       params.Limit,
			"offset":      params.Offset,
		})
		return
	}

	// Non-paginated path: return all routing rules
	rules, err := h.cfg.StoreFromRequestCtx(ctx).GetRoutingRules(ctx)
	if err != nil {
		logger.Error("failed to retrieve routing rules: %v", err)
		SendError(ctx, 500, "Failed to retrieve routing rules")
		return
	}
	SendJSON(ctx, map[string]interface{}{
		"rules":       rules,
		"count":       len(rules),
		"total_count": len(rules),
		"limit":       len(rules),
		"offset":      0,
	})
}

// getRoutingRule retrieves a single routing rule by ID from database
func (h *GovernanceHandler) getRoutingRule(ctx *fasthttp.RequestCtx) {
	ruleID := ctx.UserValue("rule_id").(string)

	var rule *configstoreTables.TableRoutingRule
	var err error

	// Check if "from_memory" query parameter is set to true
	fromMemory := string(ctx.QueryArgs().Peek("from_memory")) == "true"
	if fromMemory {
		gd := h.governanceManager.GetGovernanceData(ctx)
		if gd == nil {
			SendError(ctx, 500, "Governance data is not available")
			return
		}
		inMemoryRules := gd.RoutingRules

		// Find rule by ID in memory
		for _, r := range inMemoryRules {
			if r.ID == ruleID {
				rule = r
				break
			}
		}
		if rule == nil {
			SendError(ctx, 404, "Routing rule not found")
			return
		}
	} else {
		rule, err = h.cfg.StoreFromRequestCtx(ctx).GetRoutingRule(ctx, ruleID)
		if err != nil {
			if errors.Is(err, configstore.ErrNotFound) {
				SendError(ctx, 404, "Routing rule not found")
				return
			}
			logger.Error("failed to get routing rule: %v", err)
			SendError(ctx, 500, "Failed to retrieve routing rule")
			return
		}
	}

	SendJSON(ctx, map[string]interface{}{
		"rule": rule,
	})
}

// createRoutingRule creates a new routing rule
func (h *GovernanceHandler) createRoutingRule(ctx *fasthttp.RequestCtx) {
	// Parse request body
	var req CreateRoutingRuleRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}

	// Validate required fields
	if req.Name == "" {
		SendError(ctx, 400, "name field is required")
		return
	}

	if err := validateRoutingOutput(req.ProviderID, req.Provider, req.KeyID); err != nil {
		SendError(ctx, 400, err.Error())
		return
	}
	if err := validateRoutingFallbacks(req.Fallbacks); err != nil {
		SendError(ctx, 400, err.Error())
		return
	}

	if err := validateRoutingAssociation(req.ScopeOrgID, req.VirtualKeyID); err != nil {
		SendError(ctx, 400, err.Error())
		return
	}

	ruleID := uuid.NewString()

	// Create routing rule
	// Handle Enabled/ChainRule: nil means use DB default (true/false), otherwise use provided value
	enabled := req.Enabled
	if enabled == nil {
		enabled = bifrost.Ptr(true)
	}
	chainRule := false // DB default
	if req.ChainRule != nil {
		chainRule = *req.ChainRule
	}
	rule := &configstoreTables.TableRoutingRule{
		ID:              ruleID,
		Name:            req.Name,
		Description:     req.Description,
		Enabled:         enabled,
		ChainRule:       chainRule,
		CelExpression:   req.CelExpression,
		ProviderID:      req.ProviderID,
		ModelID:         req.ModelID,
		Provider:        req.Provider,
		Model:           req.Model,
		KeyID:           req.KeyID,
		ScopeOrgID:      req.ScopeOrgID,
		VirtualKeyID:    req.VirtualKeyID,
		Priority:        req.Priority,
		ParsedFallbacks: req.Fallbacks,
		ParsedQuery:     req.Query,
	}
	if err := rule.NormalizeRoutingAssociation(); err != nil {
		SendError(ctx, 400, err.Error())
		return
	}

	// Create in database
	if err := h.cfg.StoreFromRequestCtx(ctx).CreateRoutingRule(ctx, rule); err != nil {
		SendError(ctx, 500, fmt.Sprintf("Failed to create routing rule: %v", err))
		return
	}

	// Update in-memory store via manager callback
	if err := h.governanceManager.ReloadRoutingRule(ctx, rule.ID); err != nil {
		SendError(ctx, 500, fmt.Sprintf("Failed to reload routing rule in memory: %v, please restart bifrost to sync with the database", err))
		return
	}

	SendJSON(ctx, map[string]interface{}{
		"message": "Routing rule created successfully",
		"rule":    rule,
	})
}

// updateRoutingRule updates an existing routing rule
func (h *GovernanceHandler) updateRoutingRule(ctx *fasthttp.RequestCtx) {
	ruleID := ctx.UserValue("rule_id").(string)

	// Parse request body
	var req UpdateRoutingRuleRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, 400, "Invalid JSON")
		return
	}

	rule, err := h.cfg.StoreFromRequestCtx(ctx).GetRoutingRule(ctx, ruleID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Routing rule not found")
			return
		}
		logger.Error("failed to get routing rule: %v", err)
		SendError(ctx, 500, "Failed to retrieve routing rule")
		return
	}

	// Update fields if provided
	if req.Name != nil && *req.Name != "" {
		rule.Name = *req.Name
	}
	if req.Description != nil {
		rule.Description = *req.Description
	}
	if req.Enabled != nil {
		rule.Enabled = req.Enabled
	}
	if req.ChainRule != nil {
		rule.ChainRule = *req.ChainRule
	}
	if req.CelExpression != nil {
		rule.CelExpression = *req.CelExpression
	}
	if req.ProviderID != nil {
		if strings.TrimSpace(*req.ProviderID) == "" {
			rule.ProviderID = nil
		} else {
			rule.ProviderID = req.ProviderID
		}
	}
	if req.ModelID != nil {
		if strings.TrimSpace(*req.ModelID) == "" {
			rule.ModelID = nil
		} else {
			rule.ModelID = req.ModelID
		}
	}
	if req.Provider != nil {
		if strings.TrimSpace(*req.Provider) == "" {
			rule.Provider = nil
		} else {
			rule.Provider = req.Provider
		}
	}
	if req.Model != nil {
		if strings.TrimSpace(*req.Model) == "" {
			rule.Model = nil
		} else {
			rule.Model = req.Model
		}
	}
	if req.KeyID != nil {
		if strings.TrimSpace(*req.KeyID) == "" {
			rule.KeyID = nil
		} else {
			rule.KeyID = req.KeyID
		}
	}
	if req.ProviderID != nil || req.ModelID != nil || req.Provider != nil || req.Model != nil || req.KeyID != nil {
		if err := validateRoutingOutput(rule.ProviderID, rule.Provider, rule.KeyID); err != nil {
			SendError(ctx, 400, err.Error())
			return
		}
	}
	if req.Priority != nil {
		rule.Priority = *req.Priority
	}
	if req.Query != nil {
		rule.ParsedQuery = req.Query
	}
	if req.Fallbacks != nil {
		if err := validateRoutingFallbacks(req.Fallbacks); err != nil {
			SendError(ctx, 400, err.Error())
			return
		}
		rule.ParsedFallbacks = req.Fallbacks
	}
	if req.ScopeOrgID != nil {
		if strings.TrimSpace(*req.ScopeOrgID) == "" {
			rule.ScopeOrgID = nil
		} else {
			rule.ScopeOrgID = req.ScopeOrgID
		}
	}
	if req.VirtualKeyID != nil {
		if strings.TrimSpace(*req.VirtualKeyID) == "" {
			rule.VirtualKeyID = nil
		} else {
			rule.VirtualKeyID = req.VirtualKeyID
		}
	}
	if err := rule.NormalizeRoutingAssociation(); err != nil {
		SendError(ctx, 400, err.Error())
		return
	}

	// Update in database
	if err := h.cfg.StoreFromRequestCtx(ctx).UpdateRoutingRule(ctx, rule); err != nil {
		SendError(ctx, 500, fmt.Sprintf("Failed to update routing rule in database: %v", err))
		return
	}

	// Update in-memory store via manager callback
	if err := h.governanceManager.ReloadRoutingRule(ctx, rule.ID); err != nil {
		SendError(ctx, 500, fmt.Sprintf("Failed to reload routing rule in memory: %v, please restart bifrost to sync with the database", err))
		return
	}

	SendJSON(ctx, map[string]interface{}{
		"message": "Routing rule updated successfully",
		"rule":    rule,
	})
}

// deleteRoutingRule deletes a routing rule
func (h *GovernanceHandler) deleteRoutingRule(ctx *fasthttp.RequestCtx) {
	ruleID := ctx.UserValue("rule_id").(string)

	// Delete from database
	if err := h.cfg.StoreFromRequestCtx(ctx).DeleteRoutingRule(ctx, ruleID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 404, "Routing rule not found")
			return
		}
		SendError(ctx, 500, fmt.Sprintf("Failed to delete routing rule from database: %v", err))
		return
	}

	// Remove from in-memory store via manager callback (non-fatal: DB already updated)
	if err := h.governanceManager.RemoveRoutingRule(ctx, ruleID); err != nil {
		logger.Error("failed to remove routing rule from memory: %v", err)
	}

	SendJSON(ctx, map[string]interface{}{
		"message": "Routing rule deleted successfully",
	})
}

// validRoutingScopes contains the allowed scope values for routing rules
var validRoutingScopes = map[string]bool{
	"global":      true,
	"org":         true,
	"virtual_key": true,
}

// validateRoutingAssociation ensures scope_org_id and virtual_key_id are not both set.
func validateRoutingAssociation(scopeOrgID, virtualKeyID *string) error {
	hasOrg := scopeOrgID != nil && strings.TrimSpace(*scopeOrgID) != ""
	hasVK := virtualKeyID != nil && strings.TrimSpace(*virtualKeyID) != ""
	if hasOrg && hasVK {
		return fmt.Errorf("scope_org_id and virtual_key_id are mutually exclusive")
	}
	return nil
}

// validateRoutingScope validates legacy scope query values (org replaces team/customer).
func validateRoutingScope(scope string) error {
	if scope == "" {
		return nil
	}
	if !validRoutingScopes[scope] {
		return fmt.Errorf("invalid scope %q: must be one of: global, org, virtual_key", scope)
	}
	return nil
}

// validateRoutingOutput ensures key_id is only set when a provider pin is set.
func validateRoutingOutput(providerID, provider, keyID *string) error {
	hasProvider := (providerID != nil && strings.TrimSpace(*providerID) != "") ||
		(provider != nil && strings.TrimSpace(*provider) != "")
	if keyID != nil && strings.TrimSpace(*keyID) != "" && !hasProvider {
		return fmt.Errorf("key_id requires provider_id or provider to be set")
	}
	return nil
}

// validateRoutingFallbacks ensures each fallback parses to a non-empty known provider via
// schemas.ParseModelString (e.g. "openai/gpt-4o", or "azure/" to use the incoming model).
func validateRoutingFallbacks(fallbacks []string) error {
	for i, fb := range fallbacks {
		if strings.TrimSpace(fb) == "" {
			return fmt.Errorf("fallbacks[%d] must not be empty", i)
		}
		provider, _ := schemas.ParseModelString(fb, "")
		if provider == "" {
			return fmt.Errorf("fallbacks[%d] %q is invalid: must use a known provider prefix (e.g. \"openai/gpt-4o\" or \"azure/\" for the incoming model)", i, fb)
		}
	}
	return nil
}

// getVirtualKeyQuota handles GET /api/governance/virtual-keys/quota
// This is a self-service endpoint — no admin auth required. Requires tenant JWT with virtualKey claim.
func (h *GovernanceHandler) getVirtualKeyQuota(ctx *fasthttp.RequestCtx) {
	vkID := governance.VirtualKeyIDFromFastHTTPContext(ctx)
	if vkID == "" {
		SendError(ctx, 401, "Missing virtual key. Authenticate with a tenant JWT that includes a virtualKey claim.")
		return
	}

	vk, err := h.cfg.StoreFromRequestCtx(ctx).GetVirtualKey(ctx, vkID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, 401, "Virtual key not found")
			return
		}
		SendError(ctx, 500, "Failed to retrieve virtual key")
		return
	}

	SendJSON(ctx, map[string]interface{}{
		"virtual_key_name": vk.Name,
		"is_active":        vk.IsActiveValue(),
		"budgets":          vk.Budgets,
		"rate_limits":      vk.RateLimits,
		"allowed_model_configs": vk.AllowedModelConfigs,
	})
}
