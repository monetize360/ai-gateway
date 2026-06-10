// Package governance provides the in-memory cache store for fast governance data access
package governance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/framework/routing"
	"gorm.io/gorm"
)

type EntityWiseBudgets map[string][]*configstoreTables.TableBudget
type EntityWiseRateLimits map[string][]*configstoreTables.TableRateLimit

// LocalGovernanceStore provides in-memory cache for governance data with fast, non-blocking access
type LocalGovernanceStore struct {
	// Core data maps using sync.Map for lock-free reads
	virtualKeys    sync.Map // string -> *VirtualKey (VK value -> VirtualKey with preloaded relationships)
	organizations  sync.Map // string -> *Organization (org ID -> Organization for hierarchy walks)
	budgets        sync.Map // string -> *Budget (Budget ID -> Budget)
	rateLimits   sync.Map // string -> *RateLimit (RateLimit ID -> RateLimit)
	modelConfigs sync.Map // string -> *ModelConfig (key: "modelName" or "modelName:provider" -> ModelConfig)
	providers    sync.Map // string -> *Provider (Provider name -> Provider with preloaded relationships)
	routingRules sync.Map // string -> []*TableRoutingRule (key: "scope:scopeID" -> rules, scopeID="" for global)

	// Last DB usages for budgets and rate limits
	LastDBUsagesBudgetsMu            sync.RWMutex       // Last DB usages for budgets
	LastDBUsagesRateLimitsRequestsMu sync.RWMutex       // Mutex for last DB usages for rate limits requests
	LastDBUsagesRateLimitsTokensMu   sync.RWMutex       // Mutex for last DB usages for rate limits tokens
	LastDBUsagesBudgets              map[string]float64 // Map for last DB usages for budgets
	LastDBUsagesRequestsRateLimits   map[string]int64   // Map for last DB usages for rate limits requests
	LastDBUsagesTokensRateLimits     map[string]int64   // Map for last DB usages for rate limits tokens

	// CEL caching layer for routing rules
	compiledRoutingPrograms sync.Map // string -> cel.Program (key: ruleID -> compiled CEL program)
	routingCELEnv           *cel.Env // Singleton CEL environment reused for all compilations

	// Config store for refresh operations
	configStore configstore.ConfigStore

	// Model catalog for cross-provider model matching (optional)
	modelCatalog *modelcatalog.ModelCatalog

	// Logger
	logger schemas.Logger

	// Incremental DB refresh watermark (UTC). Zero until the first full load completes.
	refreshMu      sync.Mutex
	lastRefreshAt  time.Time
}

type GovernanceData struct {
	VirtualKeys   map[string]*configstoreTables.TableVirtualKey   `json:"virtual_keys"`
	Organizations map[string]*configstoreTables.TableOrganization `json:"organizations"`
	Users         map[string]*UserGovernance                      `json:"users"` // User-level governance (enterprise-only)
	Budgets      map[string]*configstoreTables.TableBudget      `json:"budgets"`
	RateLimits   map[string]*configstoreTables.TableRateLimit   `json:"rate_limits"`
	RoutingRules map[string]*configstoreTables.TableRoutingRule `json:"routing_rules"`
	ModelConfigs []*configstoreTables.TableModelConfig          `json:"model_configs"`
	Providers    []*configstoreTables.TableProvider             `json:"providers"`
}

// BusinessUnitGovernance holds in-memory budget and rate limit data for a business unit
type BusinessUnitGovernance struct {
	BudgetID    *string
	RateLimitID *string
}

// UserGovernance holds governance data for a user (enterprise-only)
type UserGovernance struct {
	BudgetID    *string `json:"budget_id,omitempty"`
	RateLimitID *string `json:"rate_limit_id,omitempty"`
}

// BudgetAndRateLimitStatus represents the current budget and rate limit usage state
// Exhaustion is determined by percent_used >= 100
type BudgetAndRateLimitStatus struct {
	BudgetPercentUsed           float64 `json:"budget_percent_used"`             // 0-100, >100 means exhausted
	RateLimitTokenPercentUsed   float64 `json:"rate_limit_token_percent_used"`   // 0-100, >100 means exhausted
	RateLimitRequestPercentUsed float64 `json:"rate_limit_request_percent_used"` // 0-100, >100 means exhausted
}

// GovernanceStore defines the interface for governance data access and policy evaluation.
//
// Error semantics contract:
//   - CheckRateLimit and CheckBudget return a non-nil error to indicate a governance/policy
//     violation (not an infrastructure/operational failure).
//   - Callers must treat any non-nil error from these methods as an explicit denial/violation
//     decision rather than a retryable infrastructure error.
//   - This contract ensures consistent behavior across implementations (e.g., in-memory,
//     DB-backed) and prevents retry loops on policy violations.
type GovernanceStore interface {
	GetGovernanceData(ctx context.Context) *GovernanceData
	GetVirtualKey(ctx context.Context, vkValue string) (*configstoreTables.TableVirtualKey, bool)
	// Budget crud.
	// UpsertBudgetConfig preserves in-memory CurrentUsage/LastReset on replacement —
	// use it for every config publish (fresh load or admin edit) so a concurrent
	// BumpBudgetUsage increment is never clobbered.
	LoadBudget(ctx context.Context, budgetID string) *configstoreTables.TableBudget
	UpsertBudgetConfig(ctx context.Context, budgetID string, config *configstoreTables.TableBudget)
	DeleteBudget(ctx context.Context, budgetID string)
	// Rate limit crud. UpsertRateLimitConfig carries in-memory counter state
	// (token + request CurrentUsage/LastReset) forward across replacements —
	// same rationale as UpsertBudgetConfig.
	LoadRateLimit(ctx context.Context, rateLimitID string) *configstoreTables.TableRateLimit
	UpsertRateLimitConfig(ctx context.Context, rateLimitID string, config *configstoreTables.TableRateLimit)
	DeleteRateLimit(ctx context.Context, rateLimitID string)
	// Provider-level governance checks
	CheckProviderBudget(ctx context.Context, request *EvaluationRequest, baselines map[string]float64) (Decision, error)
	CheckProviderRateLimit(ctx context.Context, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error)
	// Model-level governance checks
	CheckModelBudget(ctx context.Context, request *EvaluationRequest, baselines map[string]float64) (Decision, error)
	CheckModelRateLimit(ctx context.Context, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error)
	// VK-level governance checks
	CheckVirtualKeyBudget(ctx context.Context, vk *configstoreTables.TableVirtualKey, request *EvaluationRequest, baselines map[string]float64) (Decision, error)
	CheckVirtualKeyRateLimit(ctx context.Context, vk *configstoreTables.TableVirtualKey, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error)
	// In-memory usage updates (for VK-level)
	UpdateVirtualKeyBudgetUsageInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, cost float64) error
	UpdateVirtualKeyRateLimitUsageInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error
	// In-memory reset checks (return items that need DB sync)
	ResetExpiredRateLimitsInMemory(ctx context.Context) []*configstoreTables.TableRateLimit
	ResetExpiredBudgetsInMemory(ctx context.Context) []*configstoreTables.TableBudget
	// DB sync for expired items
	ResetExpiredRateLimits(ctx context.Context, resetRateLimits []*configstoreTables.TableRateLimit) error
	ResetExpiredBudgets(ctx context.Context, resetBudgets []*configstoreTables.TableBudget) error
	// Provider and model-level usage updates (combined)
	UpdateProviderAndModelBudgetUsageInMemory(ctx context.Context, model string, provider schemas.ModelProvider, cost float64) error
	UpdateProviderAndModelRateLimitUsageInMemory(ctx context.Context, model string, provider schemas.ModelProvider, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error
	// Dump operations
	DumpRateLimits(ctx context.Context, tokenBaselines map[string]int64, requestBaselines map[string]int64) error
	DumpBudgets(ctx context.Context, baselines map[string]float64) error
	// In-memory CRUD operations
	CreateVirtualKeyInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey)
	UpdateVirtualKeyInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, budgetBaselines map[string]float64, rateLimitTokensBaselines map[string]int64, rateLimitRequestsBaselines map[string]int64)
	DeleteVirtualKeyInMemory(ctx context.Context, vkID string)
	// Org hierarchy governance checks (walks org → parent → … → root)
	CheckOrgHierarchyBudget(ctx context.Context, orgID string, request *EvaluationRequest, baselines map[string]float64) (Decision, error)
	CheckOrgHierarchyRateLimit(ctx context.Context, orgID string, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error)
	// User governance in-memory operations (enterprise-only, but interface defined here for compatibility)
	GetUserGovernance(ctx context.Context, userID string) (*UserGovernance, bool)
	CreateUserGovernanceInMemory(ctx context.Context, userID string, budget *configstoreTables.TableBudget, rateLimit *configstoreTables.TableRateLimit)
	UpdateUserGovernanceInMemory(ctx context.Context, userID string, budget *configstoreTables.TableBudget, rateLimit *configstoreTables.TableRateLimit)
	DeleteUserGovernanceInMemory(ctx context.Context, userID string)
	// User-level governance checks (enterprise-only)
	CheckUserBudget(ctx context.Context, userID string, request *EvaluationRequest, baselines map[string]float64) (Decision, error)
	CheckUserRateLimit(ctx context.Context, userID string, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error)
	UpdateUserBudgetUsageInMemory(ctx context.Context, userID string, cost float64) error
	UpdateUserRateLimitUsageInMemory(ctx context.Context, userID string, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error
	// Model config in-memory operations
	UpdateModelConfigInMemory(ctx context.Context, mc *configstoreTables.TableModelConfig) *configstoreTables.TableModelConfig
	DeleteModelConfigInMemory(ctx context.Context, mcID string)
	// Provider in-memory operations
	UpdateProviderInMemory(ctx context.Context, provider *configstoreTables.TableProvider) *configstoreTables.TableProvider
	DeleteProviderInMemory(ctx context.Context, providerName string)
	// Routing Rules CEL caching
	GetRoutingProgram(ctx context.Context, rule *configstoreTables.TableRoutingRule) (cel.Program, error)
	// Budget and rate limit status queries for routing with baseline support
	GetBudgetAndRateLimitStatus(ctx context.Context, model string, provider schemas.ModelProvider, vk *configstoreTables.TableVirtualKey, budgetBaselines map[string]float64, tokenBaselines map[string]int64, requestBaselines map[string]int64) *BudgetAndRateLimitStatus
	// Routing Rules CRUD
	HasRoutingRules(ctx context.Context) bool
	GetAllRoutingRules(ctx context.Context) []*configstoreTables.TableRoutingRule
	GetScopedRoutingRules(ctx context.Context, scope string, scopeID string) []*configstoreTables.TableRoutingRule
	UpdateRoutingRuleInMemory(ctx context.Context, rule *configstoreTables.TableRoutingRule) error
	DeleteRoutingRuleInMemory(ctx context.Context, id string) error
	// CollectApplicableGovernanceIDs returns the budget and rate-limit IDs that
	// govern a request for the given virtual key, provider, and model. The
	// returned IDs are attached to log entries so that ghost-node usage
	// reconciliation can attribute cost and tokens to the correct governance
	// entities.
	CollectApplicableGovernanceIDs(ctx context.Context, virtualKey string, provider schemas.ModelProvider, model string) (budgetIDs []string, rateLimitIDs []string)
	CollectOrgAncestorIDs(orgID string) []string
}

// NewLocalGovernanceStore creates a new in-memory governance store
// The modelCatalog parameter is optional (can be nil) and enables cross-provider model matching
// for governance lookups (e.g., "openai/gpt-4o" matching config for "gpt-4o").
func NewLocalGovernanceStore(ctx context.Context, logger schemas.Logger, configStore configstore.ConfigStore, governanceConfig *configstore.GovernanceConfig, modelCatalog *modelcatalog.ModelCatalog) (*LocalGovernanceStore, error) {
	// Create singleton CEL environment once for all routing rule compilations
	env, err := createCELEnvironment()
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	store := &LocalGovernanceStore{
		configStore:                    configStore,
		logger:                         logger,
		routingCELEnv:                  env,
		modelCatalog:                   modelCatalog,
		LastDBUsagesBudgets:            make(map[string]float64),
		LastDBUsagesRequestsRateLimits: make(map[string]int64),
		LastDBUsagesTokensRateLimits:   make(map[string]int64),
	}

	if configStore != nil {
		// Load initial data from database
		if err := store.loadFromDatabase(ctx); err != nil {
			return nil, fmt.Errorf("failed to load initial data: %w", err)
		}
	} else {
		if err := store.loadFromConfigMemory(ctx, governanceConfig); err != nil {
			return nil, fmt.Errorf("failed to load governance data from config memory: %w", err)
		}
	}

	store.logger.Info("governance store initialized successfully")
	return store, nil
}

// RefreshFromDatabase reloads governance entities from the backing config store.
// After the initial full load, only rows with updated_at within the refresh window
// are fetched and merged into the in-memory maps. Callers should flush in-memory
// usage counters to the database before refreshing when usage tracking is active.
func (gs *LocalGovernanceStore) RefreshFromDatabase(ctx context.Context) error {
	if gs.configStore == nil {
		return fmt.Errorf("config store is not configured")
	}

	refreshStartedAt := time.Now().UTC()
	gs.refreshMu.Lock()
	since := gs.lastRefreshAt
	gs.refreshMu.Unlock()

	if since.IsZero() {
		if err := gs.loadFromDatabase(ctx); err != nil {
			return err
		}
		gs.refreshMu.Lock()
		gs.lastRefreshAt = refreshStartedAt
		gs.refreshMu.Unlock()
		return nil
	}

	watermark := configstore.NormalizeRefreshSince(since).Add(-configstore.RefreshOverlap)
	delta, err := gs.configStore.GetGovernanceRefreshDelta(ctx, watermark)
	if err != nil {
		return err
	}
	if delta.IsEmpty() {
		gs.refreshMu.Lock()
		gs.lastRefreshAt = refreshStartedAt
		gs.refreshMu.Unlock()
		return nil
	}

	gs.applyGovernanceRefreshDelta(ctx, delta)

	gs.refreshMu.Lock()
	gs.lastRefreshAt = refreshStartedAt
	gs.refreshMu.Unlock()
	return nil
}

// LoadBudget loads a budget by its ID from the local store.
func (gs *LocalGovernanceStore) LoadBudget(ctx context.Context, budgetID string) *configstoreTables.TableBudget {
	if budget, ok := gs.budgets.Load(budgetID); ok {
		if b, ok := budget.(*configstoreTables.TableBudget); ok {
			return b
		}
	}
	return nil
}

// UpsertBudgetConfig publishes a budget config under budgetID, preserving the
// in-memory CurrentUsage and LastReset from any prior snapshot so a concurrent
// BumpBudgetUsage or ResetBudgetAt is never clobbered by a config replacement.
// First-writes (no prior entry) are handled via sync.Map.LoadOrStore so
// simultaneous first-writers collapse to a single insertion and the late
// arrival re-enters the CAS loop against the winner's snapshot.
//
// This method replaces the former blind StoreBudget: every caller installing
// a budget — whether fresh load or config replacement — should funnel through
// here so counters are never clobbered by an admin edit racing with a usage
// increment.
func (gs *LocalGovernanceStore) UpsertBudgetConfig(ctx context.Context, budgetID string, config *configstoreTables.TableBudget) {
	if config == nil {
		return
	}
	for {
		raw, exists := gs.budgets.Load(budgetID)
		if !exists {
			if _, loaded := gs.budgets.LoadOrStore(budgetID, config); !loaded {
				return
			}
			continue
		}
		old, ok := raw.(*configstoreTables.TableBudget)
		if !ok || old == nil {
			gs.budgets.Store(budgetID, config)
			return
		}
		merged := *config
		merged.CurrentUsage = old.CurrentUsage
		merged.LastReset = old.LastReset
		if gs.budgets.CompareAndSwap(budgetID, raw, &merged) {
			return
		}
	}
}

// DeleteBudget deletes a budget from the local store.
func (gs *LocalGovernanceStore) DeleteBudget(ctx context.Context, budgetID string) {
	gs.budgets.Delete(budgetID)
	// Clean up LastDB baselines so the gossip delta doesn't carry stale entries.
	gs.LastDBUsagesBudgetsMu.Lock()
	delete(gs.LastDBUsagesBudgets, budgetID)
	gs.LastDBUsagesBudgetsMu.Unlock()
}

// SetBudgetDBBaseline records the DB-authoritative usage for a budget so that
// gossip delta calculations (CurrentUsage - LastDBUsage) start from the correct
// base. Must be called whenever a budget with non-zero usage is loaded into
// memory outside of the initial loadFromDatabase path (e.g., access-profile
// propagation that preserves usage under a new ID).
func (gs *LocalGovernanceStore) SetBudgetDBBaseline(budgetID string, currentUsage float64) {
	gs.LastDBUsagesBudgetsMu.Lock()
	gs.LastDBUsagesBudgets[budgetID] = currentUsage
	gs.LastDBUsagesBudgetsMu.Unlock()
}

// LoadRateLimit loads a rate limit by its ID from the local store.
func (gs *LocalGovernanceStore) LoadRateLimit(ctx context.Context, rateLimitID string) *configstoreTables.TableRateLimit {
	if rateLimit, ok := gs.rateLimits.Load(rateLimitID); ok {
		if rl, ok := rateLimit.(*configstoreTables.TableRateLimit); ok {
			return rl
		}
	}
	return nil
}

// UpsertRateLimitConfig publishes a rate-limit config under rateLimitID,
// preserving in-memory token and request counter state (TokenCurrentUsage /
// TokenLastReset / RequestCurrentUsage / RequestLastReset) from any prior
// snapshot. Same CAS-retry contract as UpsertBudgetConfig.
func (gs *LocalGovernanceStore) UpsertRateLimitConfig(ctx context.Context, rateLimitID string, config *configstoreTables.TableRateLimit) {
	if config == nil {
		return
	}
	for {
		raw, exists := gs.rateLimits.Load(rateLimitID)
		if !exists {
			if _, loaded := gs.rateLimits.LoadOrStore(rateLimitID, config); !loaded {
				return
			}
			continue
		}
		old, ok := raw.(*configstoreTables.TableRateLimit)
		if !ok || old == nil {
			gs.rateLimits.Store(rateLimitID, config)
			return
		}
		merged := *config
		merged.TokenCurrentUsage = old.TokenCurrentUsage
		merged.TokenLastReset = old.TokenLastReset
		merged.RequestCurrentUsage = old.RequestCurrentUsage
		merged.RequestLastReset = old.RequestLastReset
		if gs.rateLimits.CompareAndSwap(rateLimitID, raw, &merged) {
			return
		}
	}
}

// DeleteRateLimit deletes a rate limit from the local store.
func (gs *LocalGovernanceStore) DeleteRateLimit(ctx context.Context, rateLimitID string) {
	gs.rateLimits.Delete(rateLimitID)
	// Clean up LastDB baselines so the gossip delta doesn't carry stale entries.
	gs.LastDBUsagesRateLimitsTokensMu.Lock()
	delete(gs.LastDBUsagesTokensRateLimits, rateLimitID)
	gs.LastDBUsagesRateLimitsTokensMu.Unlock()
	gs.LastDBUsagesRateLimitsRequestsMu.Lock()
	delete(gs.LastDBUsagesRequestsRateLimits, rateLimitID)
	gs.LastDBUsagesRateLimitsRequestsMu.Unlock()
}

// SetRateLimitDBBaseline records the DB-authoritative usage for a rate limit so
// that gossip delta calculations (TokenCurrentUsage - LastDBTokenUsage) start
// from the correct base. Must be called whenever a rate limit with non-zero
// usage is loaded into memory outside of the initial loadFromDatabase path
// (e.g., access-profile propagation that preserves usage under a new ID).
func (gs *LocalGovernanceStore) SetRateLimitDBBaseline(rateLimitID string, tokenUsage int64, requestUsage int64) {
	gs.LastDBUsagesRateLimitsTokensMu.Lock()
	gs.LastDBUsagesTokensRateLimits[rateLimitID] = tokenUsage
	gs.LastDBUsagesRateLimitsTokensMu.Unlock()
	gs.LastDBUsagesRateLimitsRequestsMu.Lock()
	gs.LastDBUsagesRequestsRateLimits[rateLimitID] = requestUsage
	gs.LastDBUsagesRateLimitsRequestsMu.Unlock()
}

// BumpBudgetUsage atomically increments CurrentUsage on the budget identified
// by budgetID and, as a side effect, zeros CurrentUsage / advances LastReset
// when the rolling ResetDuration has elapsed. Uses sync.Map.CompareAndSwap so
// concurrent callers on the same budget never drop increments — a lost CAS
// retries against the winner's snapshot. No-op when the budget is absent.
//
// This is the serialisation point for every usage increment: callers MUST
// funnel through this method (directly or via one of the higher-level
// Update*BudgetUsageInMemory wrappers) rather than doing a plain
// Load → clone → mutate → Store, which races.
func (gs *LocalGovernanceStore) BumpBudgetUsage(ctx context.Context, budgetID string, cost float64) error {
	for {
		raw, exists := gs.budgets.Load(budgetID)
		if !exists || raw == nil {
			return nil
		}
		old, ok := raw.(*configstoreTables.TableBudget)
		if !ok || old == nil {
			return nil
		}
		clone := *old
		now := time.Now()
		if clone.ResetDuration != "" {
			if duration, err := configstoreTables.ParseDuration(clone.ResetDuration); err == nil {
				if now.Sub(clone.LastReset) >= duration {
					clone.CurrentUsage = 0
					clone.LastReset = now
				}
			}
		}
		clone.CurrentUsage += cost
		if gs.budgets.CompareAndSwap(budgetID, raw, &clone) {
			return nil
		}
	}
}

// BumpRateLimitUsage atomically increments the token and/or request counters on
// the rate limit identified by rateLimitID and, as a side effect, zeros the
// relevant counter / advances its LastReset when the rolling
// TokenResetDuration / RequestResetDuration has elapsed. Same CAS-retry
// contract as BumpBudgetUsage — no increment is ever dropped under
// concurrent callers. No-op when the rate limit is absent.
func (gs *LocalGovernanceStore) BumpRateLimitUsage(ctx context.Context, rateLimitID string, tokensUsed int64, shouldUpdateTokens, shouldUpdateRequests bool) error {
	for {
		raw, exists := gs.rateLimits.Load(rateLimitID)
		if !exists || raw == nil {
			return nil
		}
		old, ok := raw.(*configstoreTables.TableRateLimit)
		if !ok || old == nil {
			return nil
		}
		clone := *old
		now := time.Now()
		if clone.TokenResetDuration != nil {
			if duration, err := configstoreTables.ParseDuration(*clone.TokenResetDuration); err == nil {
				if now.Sub(clone.TokenLastReset) >= duration {
					clone.TokenCurrentUsage = 0
					clone.TokenLastReset = now
				}
			}
		}
		if clone.RequestResetDuration != nil {
			if duration, err := configstoreTables.ParseDuration(*clone.RequestResetDuration); err == nil {
				if now.Sub(clone.RequestLastReset) >= duration {
					clone.RequestCurrentUsage = 0
					clone.RequestLastReset = now
				}
			}
		}
		if shouldUpdateTokens {
			clone.TokenCurrentUsage += tokensUsed
		}
		if shouldUpdateRequests {
			clone.RequestCurrentUsage++
		}
		if gs.rateLimits.CompareAndSwap(rateLimitID, raw, &clone) {
			return nil
		}
	}
}

// ResetBudgetAt atomically zeros the budget's CurrentUsage and advances its
// LastReset to newLastReset, provided the currently-stored budget has an
// older LastReset. Returns the reset budget and true when the CAS succeeds;
// (nil, false) if the budget is absent or another writer has already advanced
// LastReset to at least newLastReset. Callers (e.g. ResetExpiredBudgetsInMemory)
// use the false return to skip the DB-persistence and reference-refresh work
// that would otherwise be redundant.
func (gs *LocalGovernanceStore) ResetBudgetAt(ctx context.Context, budgetID string, newLastReset time.Time) (*configstoreTables.TableBudget, bool) {
	for {
		raw, exists := gs.budgets.Load(budgetID)
		if !exists || raw == nil {
			return nil, false
		}
		old, ok := raw.(*configstoreTables.TableBudget)
		if !ok || old == nil {
			return nil, false
		}
		if !old.LastReset.Before(newLastReset) {
			// Someone else already advanced LastReset past ours, or the reset
			// window hasn't actually opened relative to the stored snapshot.
			return nil, false
		}
		clone := *old
		clone.CurrentUsage = 0
		clone.LastReset = newLastReset
		if gs.budgets.CompareAndSwap(budgetID, raw, &clone) {
			return &clone, true
		}
	}
}

// ResetRateLimitAt atomically resets one or both rate-limit counters on the
// rate limit identified by rateLimitID. A non-nil tokenNewLastReset resets the
// token counter and advances TokenLastReset; similarly for
// requestNewLastReset. Each reset is conditional on the corresponding
// LastReset currently being strictly older than the supplied target, so
// concurrent resetters collapse into a single successful write. Returns the
// updated snapshot and true when at least one counter was reset; (nil, false)
// otherwise.
func (gs *LocalGovernanceStore) ResetRateLimitAt(ctx context.Context, rateLimitID string, tokenNewLastReset, requestNewLastReset *time.Time) (*configstoreTables.TableRateLimit, bool) {
	if tokenNewLastReset == nil && requestNewLastReset == nil {
		return nil, false
	}
	for {
		raw, exists := gs.rateLimits.Load(rateLimitID)
		if !exists || raw == nil {
			return nil, false
		}
		old, ok := raw.(*configstoreTables.TableRateLimit)
		if !ok || old == nil {
			return nil, false
		}
		clone := *old
		didReset := false
		if tokenNewLastReset != nil && old.TokenLastReset.Before(*tokenNewLastReset) {
			clone.TokenCurrentUsage = 0
			clone.TokenLastReset = *tokenNewLastReset
			didReset = true
		}
		if requestNewLastReset != nil && old.RequestLastReset.Before(*requestNewLastReset) {
			clone.RequestCurrentUsage = 0
			clone.RequestLastReset = *requestNewLastReset
			didReset = true
		}
		if !didReset {
			return nil, false
		}
		if gs.rateLimits.CompareAndSwap(rateLimitID, raw, &clone) {
			return &clone, true
		}
	}
}

// GetGovernanceData returns a snapshot of the current governance data.
func (gs *LocalGovernanceStore) GetGovernanceData(ctx context.Context) *GovernanceData {
	refreshVKAssociations := func(vk *configstoreTables.TableVirtualKey) {
		if vk == nil {
			return
		}
		// Cross-reference live budget/rate limit from standalone maps
		// (usage updates clone into budgets/rateLimits maps, so embedded pointers go stale)
		// Hydrate multi-budgets from live sync.Map
		if len(vk.Budgets) > 0 {
			liveBudgets := make([]configstoreTables.TableBudget, 0, len(vk.Budgets))
			for _, b := range vk.Budgets {
				if lb, exists := gs.budgets.Load(b.ID); exists && lb != nil {
					if budget, ok := lb.(*configstoreTables.TableBudget); ok {
						liveBudgets = append(liveBudgets, *budget)
					}
				}
			}
			vk.Budgets = liveBudgets
		}
		if len(vk.RateLimits) > 0 {
			hydrateVirtualKeyRateLimits(vk, gs)
		}
		if len(vk.ProviderConfigs) > 0 {
			configs := make([]configstoreTables.TableVirtualKeyProviderConfig, len(vk.ProviderConfigs))
			copy(configs, vk.ProviderConfigs)
			for i := range configs {
				// Hydrate provider config multi-budgets
				if len(configs[i].Budgets) > 0 {
					liveBudgets := make([]configstoreTables.TableBudget, 0, len(configs[i].Budgets))
					for _, b := range configs[i].Budgets {
						if lb, exists := gs.budgets.Load(b.ID); exists && lb != nil {
							if budget, ok := lb.(*configstoreTables.TableBudget); ok {
								liveBudgets = append(liveBudgets, *budget)
							}
						}
					}
					configs[i].Budgets = liveBudgets
				}
				configs[i].RateLimits = hydrateRateLimitSlice(configs[i].RateLimits, gs)
			}
			vk.ProviderConfigs = configs
		}
	}

	virtualKeys := make(map[string]*configstoreTables.TableVirtualKey)
	gs.virtualKeys.Range(func(key, value interface{}) bool {
		vk, ok := value.(*configstoreTables.TableVirtualKey)
		if !ok || vk == nil {
			return true // continue
		}
		clone := *vk
		refreshVKAssociations(&clone)
		virtualKeys[key.(string)] = &clone
		return true // continue iteration
	})
	organizations := make(map[string]*configstoreTables.TableOrganization)
	gs.organizations.Range(func(key, value interface{}) bool {
		org, ok := value.(*configstoreTables.TableOrganization)
		if !ok || org == nil {
			return true
		}
		clone := *org
		organizations[key.(string)] = &clone
		return true
	})
	budgets := make(map[string]*configstoreTables.TableBudget)
	gs.budgets.Range(func(key, value interface{}) bool {
		budget, ok := value.(*configstoreTables.TableBudget)
		if !ok || budget == nil {
			return true // continue
		}
		budgets[key.(string)] = budget
		return true // continue iteration
	})
	rateLimits := make(map[string]*configstoreTables.TableRateLimit)
	gs.rateLimits.Range(func(key, value interface{}) bool {
		rateLimit, ok := value.(*configstoreTables.TableRateLimit)
		if !ok || rateLimit == nil {
			return true // continue
		}
		rateLimits[key.(string)] = rateLimit
		return true // continue iteration
	})
	routingRules := make(map[string]*configstoreTables.TableRoutingRule)
	gs.routingRules.Range(func(key, value any) bool {
		rules, ok := value.([]*configstoreTables.TableRoutingRule)
		if !ok || rules == nil {
			return true // continue
		}
		// Flatten the rules array (stored as []*TableRoutingRule by scope:scopeID)
		for _, rule := range rules {
			if rule != nil {
				routingRules[rule.ID] = rule
			}
		}
		return true // continue iteration
	})
	var modelConfigsList []*configstoreTables.TableModelConfig
	gs.modelConfigs.Range(func(key, value any) bool {
		mc, ok := value.(*configstoreTables.TableModelConfig)
		if !ok || mc == nil {
			return true // continue
		}
		// Cross-reference live budget/rate limit from standalone maps
		// (usage updates clone into budgets/rateLimits maps, so embedded pointers go stale)
		clone := *mc
		hydrateModelConfigGovernance(&clone, gs)
		modelConfigsList = append(modelConfigsList, &clone)
		return true // continue iteration
	})
	var providersList []*configstoreTables.TableProvider
	gs.providers.Range(func(key, value interface{}) bool {
		p, ok := value.(*configstoreTables.TableProvider)
		if !ok || p == nil {
			return true // continue
		}
		// Cross-reference live budget/rate limit from standalone maps
		clone := *p
		hydrateProviderGovernance(&clone, gs)
		providersList = append(providersList, &clone)
		return true // continue iteration
	})
	// Sort slice fields by CreatedAt so responses are sent in consistent order
	sort.Slice(modelConfigsList, func(i, j int) bool {
		return modelConfigsList[i].CreatedAt.Before(modelConfigsList[j].CreatedAt)
	})
	sort.Slice(providersList, func(i, j int) bool {
		return providersList[i].CreatedAt.Before(providersList[j].CreatedAt)
	})
	return &GovernanceData{
		VirtualKeys:   virtualKeys,
		Organizations: organizations,
		Budgets:       budgets,
		RateLimits:    rateLimits,
		RoutingRules:  routingRules,
		ModelConfigs:  modelConfigsList,
		Providers:     providersList,
	}
}

// storeVirtualKey indexes a virtual key by governance_virtual_keys.id (lock-free).
func (gs *LocalGovernanceStore) storeVirtualKey(vk *configstoreTables.TableVirtualKey) {
	if vk == nil || vk.ID == "" {
		return
	}
	gs.virtualKeys.Store(vk.ID, vk)
}

// GetVirtualKey retrieves a virtual key by ID (lock-free).
func (gs *LocalGovernanceStore) GetVirtualKey(ctx context.Context, vkKey string) (*configstoreTables.TableVirtualKey, bool) {
	value, exists := gs.virtualKeys.Load(vkKey)
	if !exists || value == nil {
		return nil, false
	}
	vk, ok := value.(*configstoreTables.TableVirtualKey)
	if !ok || vk == nil {
		return nil, false
	}
	return vk, true
}

// CheckRateLimit checks rate limits for tokens and requests across categories
func (gs *LocalGovernanceStore) CheckRateLimit(ctx context.Context, entityWiseRateLimits EntityWiseRateLimits, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	for entity, rateLimits := range entityWiseRateLimits {
		for _, rateLimit := range rateLimits {
			var violations []string
			// Check if rate limit needs reset (in-memory check)
			// Track which limits are expired so we can skip only those specific checks
			tokenLimitExpired := false
			if rateLimit.TokenResetDuration != nil {
				if duration, err := configstoreTables.ParseDuration(*rateLimit.TokenResetDuration); err == nil {
					if time.Since(rateLimit.TokenLastReset) >= duration {
						// Token rate limit expired but hasn't been reset yet - skip token check only
						tokenLimitExpired = true
					}
				}
			}
			requestLimitExpired := false
			if rateLimit.RequestResetDuration != nil {
				if duration, err := configstoreTables.ParseDuration(*rateLimit.RequestResetDuration); err == nil {
					if time.Since(rateLimit.RequestLastReset) >= duration {
						// Request rate limit expired but hasn't been reset yet - skip request check only
						requestLimitExpired = true
					}
				}
			}

			tokensBaseline, exists := tokensBaselines[rateLimit.ID]
			if !exists {
				tokensBaseline = 0
			}
			requestsBaseline, exists := requestsBaselines[rateLimit.ID]
			if !exists {
				requestsBaseline = 0
			}

			// Token limits - check if total usage (local + remote baseline) exceeds limit
			// Skip this check if token limit has expired
			if !tokenLimitExpired && rateLimit.TokenMaxLimit != nil && rateLimit.TokenCurrentUsage+tokensBaseline >= *rateLimit.TokenMaxLimit {
				duration := "unknown"
				if rateLimit.TokenResetDuration != nil {
					duration = *rateLimit.TokenResetDuration
				}
				violations = append(violations, fmt.Sprintf("token limit exceeded (%d/%d, resets every %s)",
					rateLimit.TokenCurrentUsage+tokensBaseline, *rateLimit.TokenMaxLimit, duration))
			}

			// Request limits - check if total usage (local + remote baseline) exceeds limit
			// Skip this check if request limit has expired
			if !requestLimitExpired && rateLimit.RequestMaxLimit != nil && rateLimit.RequestCurrentUsage+requestsBaseline >= *rateLimit.RequestMaxLimit {
				duration := "unknown"
				if rateLimit.RequestResetDuration != nil {
					duration = *rateLimit.RequestResetDuration
				}
				violations = append(violations, fmt.Sprintf("request limit exceeded (%d/%d, resets every %s)",
					rateLimit.RequestCurrentUsage+requestsBaseline, *rateLimit.RequestMaxLimit, duration))
			}

			if len(violations) > 0 {
				// Determine specific violation type
				decision := DecisionRateLimited // Default to general rate limited decision
				if len(violations) == 1 {
					if strings.Contains(violations[0], "token") {
						decision = DecisionTokenLimited // More specific violation type
					} else if strings.Contains(violations[0], "request") {
						decision = DecisionRequestLimited // More specific violation type
					}
				}
				return decision, fmt.Errorf("rate limit violated for %s: %s", entity, violations)
			}
		}
	}
	return DecisionAllow, nil
}

// Generic check budget method
// The idea is to keep this as a common method for checking all budgets. The entire business logic resides in here
func (gs *LocalGovernanceStore) CheckBudget(ctx context.Context, entityWiseBudgets EntityWiseBudgets, baselines map[string]float64) (Decision, error) {
	// Check each budget in hierarchy order using in-memory data
	for entity, budgets := range entityWiseBudgets {
		for _, budget := range budgets { // Check if budget needs reset (in-memory check)
			if budget.ResetDuration != "" {
				if duration, err := configstoreTables.ParseDuration(budget.ResetDuration); err == nil {
					if time.Since(budget.LastReset) >= duration {
						// Budget expired but hasn't been reset yet - treat as reset
						// Note: actual reset will happen in post-hook via AtomicBudgetUpdate
						gs.logger.Debug("LocalStore CheckBudget: Budget %s (%s) expired, skipping check", budget.ID, entity)
						continue // Skip budget check for expired budgets
					}
				}
			}
			baseline, exists := baselines[budget.ID]
			if !exists {
				baseline = 0
			}
			gs.logger.Debug("LocalStore CheckBudget: Checking %s budget %s: local=%.4f, remote=%.4f, total=%.4f, limit=%.4f",
				entity, budget.ID, budget.CurrentUsage, baseline, budget.CurrentUsage+baseline, budget.MaxLimit)
			// Check if current usage (local + remote baseline) exceeds budget limit
			if budget.CurrentUsage+baseline >= budget.MaxLimit {
				gs.logger.Debug("LocalStore CheckBudget: Budget %s EXCEEDED", budget.ID)
				return DecisionBudgetExceeded, fmt.Errorf("%s budget exceeded: %.4f >= %.4f dollars",
					entity, budget.CurrentUsage+baseline, budget.MaxLimit)
			}
		}
	}
	return DecisionAllow, nil
}

// CheckVirtualKeyBudget performs virtual key level budget checking using in-memory store data (lock-free for high performance)
func (gs *LocalGovernanceStore) CheckVirtualKeyBudget(ctx context.Context, vk *configstoreTables.TableVirtualKey, request *EvaluationRequest, baselines map[string]float64) (Decision, error) {
	if vk == nil {
		return DecisionVirtualKeyNotFound, fmt.Errorf("virtual key cannot be nil")
	}
	// This is to prevent nil pointer dereference
	if baselines == nil {
		baselines = map[string]float64{}
	}
	// Extract provider from request
	var provider schemas.ModelProvider
	if request != nil {
		provider = request.Provider
	}
	// Use helper to collect budgets and their names (lock-free)
	budgetsWithCategories := gs.collectBudgetsFromHierarchy(ctx, vk, provider)
	gs.logger.Debug("LocalStore CheckBudget: Received %d baselines from remote nodes", len(baselines))
	for budgetID, baseline := range baselines {
		gs.logger.Debug("  - Baseline for budget %s: %.4f", budgetID, baseline)
	}
	return gs.CheckBudget(ctx, budgetsWithCategories, baselines)
}

// CheckProviderBudget performs budget checking for provider-level configs (lock-free for high performance)
func (gs *LocalGovernanceStore) CheckProviderBudget(ctx context.Context, request *EvaluationRequest, baselines map[string]float64) (Decision, error) {
	// This is to prevent nil pointer dereference
	if baselines == nil {
		baselines = map[string]float64{}
	}
	// Extract provider from request
	var provider schemas.ModelProvider
	if request != nil {
		provider = request.Provider
	}
	// Get provider config
	providerKey := string(provider)
	value, exists := gs.providers.Load(providerKey)
	if !exists || value == nil {
		// No provider config found, allow request
		return DecisionAllow, nil
	}
	providerTable, ok := value.(*configstoreTables.TableProvider)
	if !ok || providerTable == nil || len(providerTable.Budgets) == 0 {
		// No budget configured for provider, allow request
		return DecisionAllow, nil
	}
	budgets := loadLiveBudgets(gs, ctx, providerTable.Budgets)
	if len(budgets) == 0 {
		return DecisionAllow, nil
	}
	return gs.CheckBudget(ctx, map[string][]*configstoreTables.TableBudget{providerKey: budgets}, baselines)
}

// CheckProviderRateLimit checks provider-level rate limits and returns evaluation result if violated
func (gs *LocalGovernanceStore) CheckProviderRateLimit(ctx context.Context, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	// Extract provider from request
	var provider schemas.ModelProvider
	if request != nil {
		provider = request.Provider
	}
	// Get provider config
	providerKey := string(provider)
	value, exists := gs.providers.Load(providerKey)
	if !exists || value == nil {
		// No provider config found, allow request
		return DecisionAllow, nil
	}
	providerTable, ok := value.(*configstoreTables.TableProvider)
	if !ok || providerTable == nil || len(providerTable.RateLimits) == 0 {
		// No rate limit configured for provider, allow request
		return DecisionAllow, nil
	}
	rateLimits := loadLiveRateLimits(gs, ctx, providerTable.RateLimits)
	if len(rateLimits) == 0 {
		return DecisionAllow, nil
	}
	return gs.CheckRateLimit(ctx, EntityWiseRateLimits{providerKey: rateLimits}, tokensBaselines, requestsBaselines)
}

// findModelOnlyConfig looks up a model-only config (no provider) with cross-provider model name normalization.
// Returns the matching config and the display name for error messages.
func (gs *LocalGovernanceStore) findModelOnlyConfig(ctx context.Context, model string) (*configstoreTables.TableModelConfig, string) {
	// If modelMatcher is available, try normalized base model name first (cross-provider matching)
	if gs.modelCatalog != nil {
		baseName := gs.modelCatalog.GetBaseModelName(model)
		if baseName != model {
			if value, exists := gs.modelConfigs.Load(baseName); exists && value != nil {
				if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil {
					return mc, baseName
				}
			}
		}
	}
	// Always try direct lookup by original model name as fallback
	if value, exists := gs.modelConfigs.Load(model); exists && value != nil {
		if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil {
			return mc, model
		}
	}
	return nil, ""
}

// CheckModelBudget performs budget checking for model-level configs (lock-free for high performance)
func (gs *LocalGovernanceStore) CheckModelBudget(ctx context.Context, request *EvaluationRequest, baselines map[string]float64) (Decision, error) {
	// This is to prevent nil pointer dereference
	if baselines == nil {
		baselines = map[string]float64{}
	}
	// Extract model and provider from request
	var model string
	var provider *schemas.ModelProvider
	if request != nil {
		model = request.Model
		if request.Provider != "" {
			provider = &request.Provider
		}
	}
	// Collect model configs to check: model+provider (if exists) AND model-only (if exists)
	entityWiseBudgets := EntityWiseBudgets{}
	// Check model+provider config first (more specific) - if provider is provided
	if provider != nil {
		key := fmt.Sprintf("%s:%s", model, string(*provider))
		if value, exists := gs.modelConfigs.Load(key); exists && value != nil {
			if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil && len(mc.Budgets) > 0 {
				if budgets := loadLiveBudgets(gs, ctx, mc.Budgets); len(budgets) > 0 {
					key := fmt.Sprintf("Model:%s:Provider:%s", mc.ModelName, *provider)
					entityWiseBudgets[key] = budgets
				}
			}
		}
	}
	// Always check model-only config (if exists) - regardless of whether model+provider config exists
	// Uses findModelOnlyConfig for cross-provider model name normalization
	if mc, _ := gs.findModelOnlyConfig(ctx, model); mc != nil && len(mc.Budgets) > 0 {
		if budgets := loadLiveBudgets(gs, ctx, mc.Budgets); len(budgets) > 0 {
			key := fmt.Sprintf("Model:%s", mc.ModelName)
			entityWiseBudgets[key] = budgets
		}
	}
	return gs.CheckBudget(ctx, entityWiseBudgets, baselines)
}

// CheckOrgHierarchyBudget checks org-level budgets walking up the parent chain.
func (gs *LocalGovernanceStore) CheckOrgHierarchyBudget(ctx context.Context, orgID string, request *EvaluationRequest, baselines map[string]float64) (Decision, error) {
	if orgID == "" {
		return DecisionAllow, nil
	}
	if baselines == nil {
		baselines = map[string]float64{}
	}
	entityWiseBudgets := make(EntityWiseBudgets)
	seen := map[string]bool{}
	gs.appendOrgHierarchyBudgets(orgID, entityWiseBudgets, seen)
	if len(entityWiseBudgets) == 0 {
		return DecisionAllow, nil
	}
	return gs.CheckBudget(ctx, entityWiseBudgets, baselines)
}

// CheckOrgHierarchyRateLimit checks org-level rate limits walking up the parent chain.
func (gs *LocalGovernanceStore) CheckOrgHierarchyRateLimit(ctx context.Context, orgID string, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	if orgID == "" {
		return DecisionAllow, nil
	}
	if tokensBaselines == nil {
		tokensBaselines = map[string]int64{}
	}
	if requestsBaselines == nil {
		requestsBaselines = map[string]int64{}
	}
	rateLimitsWithCategories := map[string][]*configstoreTables.TableRateLimit{}
	seen := map[string]bool{}
	gs.appendOrgHierarchyRateLimits(orgID, rateLimitsWithCategories, seen)
	if len(rateLimitsWithCategories) == 0 {
		return DecisionAllow, nil
	}
	return gs.CheckRateLimit(ctx, rateLimitsWithCategories, tokensBaselines, requestsBaselines)
}

// CheckUserBudget checks if user's budget allows the request (enterprise-only)
// Community build: silent no-op so user-governance absence never silently denies requests.
func (gs *LocalGovernanceStore) CheckUserBudget(ctx context.Context, userID string, request *EvaluationRequest, baselines map[string]float64) (Decision, error) {
	return DecisionAllow, nil
}

// CheckModelRateLimit checks model-level rate limits and returns evaluation result if violated
func (gs *LocalGovernanceStore) CheckModelRateLimit(ctx context.Context, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	// This is to prevent nil pointer dereference
	if tokensBaselines == nil {
		tokensBaselines = map[string]int64{}
	}
	if requestsBaselines == nil {
		requestsBaselines = map[string]int64{}
	}
	// Extract model and provider from request
	var model string
	var provider *schemas.ModelProvider
	if request != nil {
		model = request.Model
		if request.Provider != "" {
			provider = &request.Provider
		}
	}
	// Collect model configs to check: model+provider (if exists) AND model-only (if exists)
	entityWiseRateLimits := make(EntityWiseRateLimits)
	// Check model+provider config first (more specific) - if provider is provided
	if provider != nil {
		key := fmt.Sprintf("%s:%s", model, string(*provider))
		if value, exists := gs.modelConfigs.Load(key); exists && value != nil {
			if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil && len(mc.RateLimits) > 0 {
				if rateLimits := loadLiveRateLimits(gs, ctx, mc.RateLimits); len(rateLimits) > 0 {
					entityWiseRateLimits[fmt.Sprintf("Model:%s:Provider:%s", model, string(*provider))] = rateLimits
				}
			}
		}
	}
	// Always check model-only config (if exists) - regardless of whether model+provider config exists
	// Uses findModelOnlyConfig for cross-provider model name normalization
	if mc, configKey := gs.findModelOnlyConfig(ctx, model); mc != nil && len(mc.RateLimits) > 0 {
		if rateLimits := loadLiveRateLimits(gs, ctx, mc.RateLimits); len(rateLimits) > 0 {
			entityWiseRateLimits[fmt.Sprintf("Model:%s", configKey)] = rateLimits
		}
	}
	return gs.CheckRateLimit(ctx, entityWiseRateLimits, tokensBaselines, requestsBaselines)
}

// CheckUserRateLimit checks if user's rate limit allows the request (enterprise-only)
// Community build: silent no-op so user-governance absence never silently denies requests.
func (gs *LocalGovernanceStore) CheckUserRateLimit(ctx context.Context, userID string, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	return DecisionAllow, nil
}

// CheckVirtualKeyRateLimit checks a virtual key  rate limit and returns evaluation result if violated (true if violated, false if not)
func (gs *LocalGovernanceStore) CheckVirtualKeyRateLimit(ctx context.Context, vk *configstoreTables.TableVirtualKey, request *EvaluationRequest, tokensBaselines map[string]int64, requestsBaselines map[string]int64) (Decision, error) {
	// Extract provider from request
	var provider schemas.ModelProvider
	if request != nil {
		provider = request.Provider
	}
	// Collect rate limits and their names from the hierarchy
	entityWiseRateLimits := gs.collectRateLimitsFromHierarchy(ctx, vk, provider)
	// This is to prevent nil pointer dereference
	if tokensBaselines == nil {
		tokensBaselines = map[string]int64{}
	}
	if requestsBaselines == nil {
		requestsBaselines = map[string]int64{}
	}
	return gs.CheckRateLimit(ctx, entityWiseRateLimits, tokensBaselines, requestsBaselines)
}

// UpdateVirtualKeyBudgetUsageInMemory performs atomic budget updates across the hierarchy (both in memory and in database)
func (gs *LocalGovernanceStore) UpdateVirtualKeyBudgetUsageInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, cost float64) error {
	if vk == nil {
		return fmt.Errorf("virtual key cannot be nil")
	}
	// Collect budget IDs using fast in-memory lookup instead of DB queries
	budgetIDs := gs.collectBudgetIDsFromMemory(ctx, vk, provider)
	for _, budgetID := range budgetIDs {
		if err := gs.BumpBudgetUsage(ctx, budgetID, cost); err != nil {
			return err
		}
	}
	return nil
}

// UpdateProviderAndModelBudgetUsageInMemory performs atomic budget updates for both provider-level and model-level configs (in memory)
func (gs *LocalGovernanceStore) UpdateProviderAndModelBudgetUsageInMemory(ctx context.Context, model string, provider schemas.ModelProvider, cost float64) error {
	// 1. Update provider-level budget (if provider is set)
	if provider != "" {
		providerKey := string(provider)
		if value, exists := gs.providers.Load(providerKey); exists && value != nil {
			if providerTable, ok := value.(*configstoreTables.TableProvider); ok && providerTable != nil {
				if err := bumpBudgetSlice(ctx, gs, providerTable.Budgets, cost); err != nil {
					return err
				}
			}
		}
	}

	// 2. Update model-level budgets
	// Check model+provider config first (more specific) - if provider is provided
	if provider != "" {
		key := fmt.Sprintf("%s:%s", model, string(provider))
		if value, exists := gs.modelConfigs.Load(key); exists && value != nil {
			if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil {
				if err := bumpBudgetSlice(ctx, gs, mc.Budgets, cost); err != nil {
					return err
				}
			}
		}
	}

	// Always check model-only config (if exists) - regardless of whether model+provider config exists
	// Uses findModelOnlyConfig for cross-provider model name normalization
	if mc, _ := gs.findModelOnlyConfig(ctx, model); mc != nil {
		if err := bumpBudgetSlice(ctx, gs, mc.Budgets, cost); err != nil {
			return err
		}
	}

	return nil
}

// UpdateUserBudgetUsageInMemory updates user's budget usage in memory (enterprise-only)
// Community build: silent no-op to avoid per-request error spam when a userID is set.
func (gs *LocalGovernanceStore) UpdateUserBudgetUsageInMemory(ctx context.Context, userID string, cost float64) error {
	return nil
}

// UpdateProviderAndModelRateLimitUsageInMemory updates rate limit counters for both provider-level and model-level rate limits.
func (gs *LocalGovernanceStore) UpdateProviderAndModelRateLimitUsageInMemory(ctx context.Context, model string, provider schemas.ModelProvider, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error {
	// 1. Update provider-level rate limit (if provider is set)
	if provider != "" {
		providerKey := string(provider)
		if value, exists := gs.providers.Load(providerKey); exists && value != nil {
			if providerTable, ok := value.(*configstoreTables.TableProvider); ok && providerTable != nil {
				if err := bumpRateLimitSlice(ctx, gs, providerTable.RateLimits, tokensUsed, shouldUpdateTokens, shouldUpdateRequests); err != nil {
					return err
				}
			}
		}
	}

	// 2. Update model-level rate limits
	// Check model+provider config first (more specific) - if provider is provided
	if provider != "" {
		key := fmt.Sprintf("%s:%s", model, string(provider))
		if value, exists := gs.modelConfigs.Load(key); exists && value != nil {
			if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil {
				if err := bumpRateLimitSlice(ctx, gs, mc.RateLimits, tokensUsed, shouldUpdateTokens, shouldUpdateRequests); err != nil {
					return err
				}
			}
		}
	}

	// Always check model-only config (if exists) - regardless of whether model+provider config exists
	// Uses findModelOnlyConfig for cross-provider model name normalization
	if mc, _ := gs.findModelOnlyConfig(ctx, model); mc != nil {
		if err := bumpRateLimitSlice(ctx, gs, mc.RateLimits, tokensUsed, shouldUpdateTokens, shouldUpdateRequests); err != nil {
			return err
		}
	}

	return nil
}

// UpdateVirtualKeyRateLimitUsageInMemory updates rate limit counters for VK-level rate limits.
func (gs *LocalGovernanceStore) UpdateVirtualKeyRateLimitUsageInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error {
	if vk == nil {
		return fmt.Errorf("virtual key cannot be nil")
	}
	// Collect rate limit IDs using fast in-memory lookup instead of DB queries
	rateLimitIDs := gs.collectRateLimitIDsFromMemory(ctx, vk, provider)
	for _, rateLimitID := range rateLimitIDs {
		if err := gs.BumpRateLimitUsage(ctx, rateLimitID, tokensUsed, shouldUpdateTokens, shouldUpdateRequests); err != nil {
			return err
		}
	}
	return nil
}

// UpdateUserRateLimitUsageInMemory updates user's rate limit usage in memory (enterprise-only)
// Community build: silent no-op to avoid per-request error spam when a userID is set.
func (gs *LocalGovernanceStore) UpdateUserRateLimitUsageInMemory(ctx context.Context, userID string, tokensUsed int64, shouldUpdateTokens bool, shouldUpdateRequests bool) error {
	return nil
}

// ResetExpiredBudgetsInMemory checks and resets budgets that have exceeded their reset duration.
// Decision of whether to reset is computed per-budget from the snapshot observed via Range; the
// actual CAS is delegated to ResetBudgetAt, which skips already-reset snapshots and never drops
// a concurrent usage increment.
func (gs *LocalGovernanceStore) ResetExpiredBudgetsInMemory(ctx context.Context) []*configstoreTables.TableBudget {
	now := time.Now()
	var resetBudgets []*configstoreTables.TableBudget
	gs.budgets.Range(func(key, value any) bool {
		budget, ok := value.(*configstoreTables.TableBudget)
		if !ok || budget == nil {
			return true
		}
		calendarAligned := budget.IsCalendarAligned
		var shouldReset bool
		var newLastReset time.Time
		if calendarAligned {
			currentPeriodStart := configstoreTables.GetCalendarPeriodStart(budget.ResetDuration, now)
			if currentPeriodStart.After(budget.LastReset) {
				shouldReset = true
				newLastReset = currentPeriodStart
			}
		} else {
			duration, err := configstoreTables.ParseDuration(budget.ResetDuration)
			if err != nil {
				gs.logger.Error("invalid budget reset duration %s: %v", budget.ResetDuration, err)
				return true
			}
			if now.Sub(budget.LastReset) >= duration {
				shouldReset = true
				newLastReset = now
			}
		}
		if !shouldReset {
			return true
		}
		resetBudget, ok := gs.ResetBudgetAt(ctx, budget.ID, newLastReset)
		if !ok {
			// Another resetter got there first, or a concurrent usage update
			// already advanced LastReset past ours; nothing to do.
			return true
		}
		oldUsage := budget.CurrentUsage
		gs.LastDBUsagesBudgetsMu.Lock()
		gs.LastDBUsagesBudgets[resetBudget.ID] = 0
		gs.LastDBUsagesBudgetsMu.Unlock()
		resetBudgets = append(resetBudgets, resetBudget)
		gs.updateBudgetReferences(ctx, resetBudget)
		gs.logger.Debug(fmt.Sprintf("Reset budget %s (was %.2f, reset to 0)",
			resetBudget.ID, oldUsage))
		return true
	})
	return resetBudgets
}

// ResetExpiredRateLimitsInMemory performs background reset of expired rate limits for both provider-level and VK-level.
// Decision of whether each counter needs resetting is computed per-rate-limit from the snapshot observed via Range;
// the actual CAS is delegated to ResetRateLimitAt, which skips already-reset snapshots and never drops a concurrent
// increment.
func (gs *LocalGovernanceStore) ResetExpiredRateLimitsInMemory(ctx context.Context) []*configstoreTables.TableRateLimit {
	now := time.Now()
	var resetRateLimits []*configstoreTables.TableRateLimit
	// resolvePeriodStart returns the next LastReset target for a counter whose
	// reset-duration setting is resetDuration and whose current LastReset is
	// lastReset. Returns nil when no reset is due (or the duration is invalid).
	resolvePeriodStart := func(resetDuration *string, calendarAligned bool, lastReset time.Time) *time.Time {
		if resetDuration == nil {
			return nil
		}
		if calendarAligned {
			period := configstoreTables.GetCalendarPeriodStart(*resetDuration, now)
			if period.After(lastReset) {
				return &period
			}
			return nil
		}
		duration, err := configstoreTables.ParseDuration(*resetDuration)
		if err != nil {
			gs.logger.Error("invalid rate limit reset duration %s: %v", *resetDuration, err)
			return nil
		}
		if now.Sub(lastReset) >= duration {
			t := now
			return &t
		}
		return nil
	}
	gs.rateLimits.Range(func(key, value any) bool {
		rateLimit, ok := value.(*configstoreTables.TableRateLimit)
		if !ok || rateLimit == nil {
			return true
		}
		calendarAligned := rateLimit.IsCalendarAligned
		tokenNewLastReset := resolvePeriodStart(rateLimit.TokenResetDuration, calendarAligned, rateLimit.TokenLastReset)
		requestNewLastReset := resolvePeriodStart(rateLimit.RequestResetDuration, calendarAligned, rateLimit.RequestLastReset)
		if tokenNewLastReset == nil && requestNewLastReset == nil {
			return true
		}
		resetRateLimit, ok := gs.ResetRateLimitAt(ctx, rateLimit.ID, tokenNewLastReset, requestNewLastReset)
		if !ok {
			return true
		}
		// Clear DB-baseline markers only for the counters we actually reset in
		// this call. Baseline locks stay independent of the primary sync.Map
		// CAS — they guard a separate map whose values just need consistency,
		// not atomicity with the counter mutation.
		if tokenNewLastReset != nil {
			gs.LastDBUsagesRateLimitsTokensMu.Lock()
			gs.LastDBUsagesTokensRateLimits[resetRateLimit.ID] = 0
			gs.LastDBUsagesRateLimitsTokensMu.Unlock()
		}
		if requestNewLastReset != nil {
			gs.LastDBUsagesRateLimitsRequestsMu.Lock()
			gs.LastDBUsagesRequestsRateLimits[resetRateLimit.ID] = 0
			gs.LastDBUsagesRateLimitsRequestsMu.Unlock()
		}
		resetRateLimits = append(resetRateLimits, resetRateLimit)
		gs.updateRateLimitReferences(ctx, resetRateLimit)
		return true
	})
	return resetRateLimits
}

// ResetExpiredBudgets checks and resets budgets that have exceeded their reset duration in database
func (gs *LocalGovernanceStore) ResetExpiredBudgets(ctx context.Context, resetBudgets []*configstoreTables.TableBudget) error {
	// Persist to database if any resets occurred using direct UPDATE to avoid overwriting config fields
	if len(resetBudgets) > 0 && gs.configStore != nil {
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			for _, budget := range resetBudgets {
				// Direct UPDATE only resets current_usage and last_reset
				// This prevents overwriting max_limit or reset_duration that may have been changed by other nodes/requests
				result := tx.WithContext(ctx).
					Session(&gorm.Session{SkipHooks: true}).
					Model(&configstoreTables.TableBudget{}).
					Where("id = ?", budget.ID).
					Updates(map[string]interface{}{
						"current_usage": budget.CurrentUsage,
						"last_reset":    budget.LastReset,
					})

				if result.Error != nil {
					return fmt.Errorf("failed to reset budget %s: %w", budget.ID, result.Error)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to persist budget resets to database: %w", err)
		}
	}

	return nil
}

// ResetExpiredRateLimits performs background reset of expired rate limits for both provider-level and VK-level in database
func (gs *LocalGovernanceStore) ResetExpiredRateLimits(ctx context.Context, resetRateLimits []*configstoreTables.TableRateLimit) error {
	if len(resetRateLimits) > 0 && gs.configStore != nil {
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			for _, rateLimit := range resetRateLimits {
				// Build update map with only the fields that were reset
				updates := make(map[string]interface{})

				// Check which fields were reset by comparing with current values
				if rateLimit.TokenCurrentUsage == 0 && rateLimit.TokenResetDuration != nil {
					updates["token_current_usage"] = 0
					updates["token_last_reset"] = rateLimit.TokenLastReset
				}
				if rateLimit.RequestCurrentUsage == 0 && rateLimit.RequestResetDuration != nil {
					updates["request_current_usage"] = 0
					updates["request_last_reset"] = rateLimit.RequestLastReset
				}

				if len(updates) > 0 {
					// Direct UPDATE only resets usage and last_reset fields
					// This prevents overwriting max_limit or reset_duration that may have been changed by other nodes/requests
					result := tx.WithContext(ctx).
						Session(&gorm.Session{SkipHooks: true}).
						Model(&configstoreTables.TableRateLimit{}).
						Where("id = ?", rateLimit.ID).
						Updates(updates)

					if result.Error != nil {
						return fmt.Errorf("failed to reset rate limit %s: %w", rateLimit.ID, result.Error)
					}
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to persist rate limit resets to database: %w", err)
		}
	}
	return nil
}

// DumpRateLimits dumps all rate limits to the database
func (gs *LocalGovernanceStore) DumpRateLimits(ctx context.Context, tokenBaselines map[string]int64, requestBaselines map[string]int64) error {
	if gs.configStore == nil {
		return nil
	}
	// This is to prevent nil pointer dereference
	if tokenBaselines == nil {
		tokenBaselines = map[string]int64{}
	}
	if requestBaselines == nil {
		requestBaselines = map[string]int64{}
	}
	// Range over ALL rate limits in memory (mirrors DumpBudgets pattern).
	// This covers rate limits from every source: virtual keys, model configs,
	// providers, teams, customers, AND access profiles — whose IDs were
	// previously missing, causing AP rate-limit usage to never reach the DB.
	type rateLimitUpdate struct {
		ID                  string
		TokenCurrentUsage   int64
		RequestCurrentUsage int64
	}
	var rateLimitUpdates []rateLimitUpdate
	gs.rateLimits.Range(func(key, value interface{}) bool {
		rateLimit, ok := value.(*configstoreTables.TableRateLimit)
		if !ok || rateLimit == nil {
			return true
		}
		update := rateLimitUpdate{
			ID:                  rateLimit.ID,
			TokenCurrentUsage:   rateLimit.TokenCurrentUsage,
			RequestCurrentUsage: rateLimit.RequestCurrentUsage,
		}
		if tokenBaseline, exists := tokenBaselines[rateLimit.ID]; exists {
			update.TokenCurrentUsage += tokenBaseline
		}
		if requestBaseline, exists := requestBaselines[rateLimit.ID]; exists {
			update.RequestCurrentUsage += requestBaseline
		}
		rateLimitUpdates = append(rateLimitUpdates, update)
		return true
	})
	sort.Slice(rateLimitUpdates, func(i, j int) bool {
		return rateLimitUpdates[i].ID < rateLimitUpdates[j].ID
	})

	// Save all updated rate limits to database using direct UPDATE to avoid overwriting config fields
	if len(rateLimitUpdates) > 0 && gs.configStore != nil {
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			for _, update := range rateLimitUpdates {
				// Direct UPDATE only updates usage fields
				// This prevents overwriting max_limit or reset_duration that may have been changed by other nodes/requests
				result := tx.WithContext(ctx).
					Session(&gorm.Session{SkipHooks: true}).
					Model(&configstoreTables.TableRateLimit{}).
					Where("id = ?", update.ID).
					Updates(map[string]interface{}{
						"token_current_usage":   update.TokenCurrentUsage,
						"request_current_usage": update.RequestCurrentUsage,
					})

				if result.Error != nil {
					return fmt.Errorf("failed to dump rate limit %s: %w", update.ID, result.Error)
				}
			}
			return nil
		}); err != nil {
			// Check if error is a deadlock (SQLSTATE 40P01 for PostgreSQL, 1213 for MySQL)
			errStr := err.Error()
			isDeadlock := strings.Contains(errStr, "deadlock") ||
				strings.Contains(errStr, "40P01") ||
				strings.Contains(errStr, "1213")

			if isDeadlock {
				// Deadlock means another node is updating the same rows - this is fine!
				// Our usage data will be synced via gossip and written in the next dump cycle
				gs.logger.Debug("Rate limit dump encountered deadlock (another node is updating) - will retry next cycle")
				return nil // Not a real error in multi-node setup
			}
			return fmt.Errorf("failed to dump rate limits to database: %w", err)
		}
	}
	return nil
}

// DumpBudgets dumps all budgets to the database
func (gs *LocalGovernanceStore) DumpBudgets(ctx context.Context, baselines map[string]float64) error {
	if gs.configStore == nil {
		return nil
	}
	// This is to prevent nil pointer dereference
	if baselines == nil {
		baselines = map[string]float64{}
	}
	budgets := make(map[string]*configstoreTables.TableBudget)
	gs.budgets.Range(func(key, value interface{}) bool {
		// Type-safe conversion
		keyStr, keyOk := key.(string)
		budget, budgetOk := value.(*configstoreTables.TableBudget)

		if keyOk && budgetOk && budget != nil {
			budgets[keyStr] = budget // Store budget by ID
		}
		return true // continue iteration
	})
	if len(budgets) > 0 && gs.configStore != nil {
		budgetIDs := make([]string, 0, len(budgets))
		for id := range budgets {
			budgetIDs = append(budgetIDs, id)
		}
		sort.Strings(budgetIDs)
		if err := gs.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			// Update each budget atomically using direct UPDATE to avoid deadlocks
			// (SELECT + Save pattern causes deadlocks when multiple instances run concurrently)
			for _, budgetID := range budgetIDs {
				inMemoryBudget := budgets[budgetID]
				// Calculate the new usage value
				newUsage := inMemoryBudget.CurrentUsage
				if baseline, exists := baselines[inMemoryBudget.ID]; exists {
					newUsage += baseline
				}

				// Direct UPDATE avoids read-then-write lock escalation that causes deadlocks
				// Use Session with SkipHooks to avoid triggering BeforeSave hook validation
				result := tx.WithContext(ctx).
					Session(&gorm.Session{SkipHooks: true}).
					Model(&configstoreTables.TableBudget{}).
					Where("id = ?", inMemoryBudget.ID).
					Update("current_usage", newUsage)

				if result.Error != nil {
					return fmt.Errorf("failed to update budget %s: %w", inMemoryBudget.ID, result.Error)
				}
			}
			return nil
		}); err != nil {
			// Check if error is a deadlock (SQLSTATE 40P01 for PostgreSQL, 1213 for MySQL)
			errStr := err.Error()
			isDeadlock := strings.Contains(errStr, "deadlock") ||
				strings.Contains(errStr, "40P01") ||
				strings.Contains(errStr, "1213")

			if isDeadlock {
				// Deadlock means another node is updating the same rows - this is fine!
				// Our usage data will be synced via gossip and written in the next dump cycle
				gs.logger.Debug("Budget dump encountered deadlock (another node is updating) - will retry next cycle")
				return nil // Not a real error in multi-node setup
			}
			return fmt.Errorf("failed to dump budgets to database: %w", err)
		}
	}
	return nil
}

// DATABASE METHODS

// loadFromDatabase loads all governance data from the database into memory
func (gs *LocalGovernanceStore) loadFromDatabase(ctx context.Context) error {
	// Load organizations for hierarchy walks
	organizations, err := gs.configStore.GetOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("failed to load organizations: %w", err)
	}

	// Load virtual keys with all relationships
	virtualKeys, err := gs.configStore.GetVirtualKeys(ctx)
	if err != nil {
		return fmt.Errorf("failed to load virtual keys: %w", err)
	}

	// Load budgets
	budgets, err := gs.configStore.GetBudgets(ctx)
	if err != nil {
		return fmt.Errorf("failed to load budgets: %w", err)
	}

	// Load rate limits
	rateLimits, err := gs.configStore.GetRateLimits(ctx)
	if err != nil {
		return fmt.Errorf("failed to load rate limits: %w", err)
	}

	// Load model configs
	modelConfigs, err := gs.configStore.GetModelConfigs(ctx)
	if err != nil {
		return fmt.Errorf("failed to load model configs: %w", err)
	}

	// Load providers with governance relationships (similar to GetModelConfigs)
	providers, err := gs.configStore.GetProviders(ctx)
	if err != nil {
		return fmt.Errorf("failed to load providers: %w", err)
	}

	// Load routing rules
	routingRules, err := gs.configStore.GetRoutingRules(ctx)
	if err != nil {
		return fmt.Errorf("failed to load routing rules: %w", err)
	}

	// Rebuild in-memory structures (lock-free)
	gs.rebuildInMemoryStructures(ctx, organizations, virtualKeys, budgets, rateLimits, modelConfigs, providers, routingRules)

	gs.refreshMu.Lock()
	gs.lastRefreshAt = time.Now().UTC()
	gs.refreshMu.Unlock()

	return nil
}

func (gs *LocalGovernanceStore) applyGovernanceRefreshDelta(ctx context.Context, delta *configstore.GovernanceRefreshDelta) {
	if delta == nil {
		return
	}

	for i := range delta.Organizations {
		org := &delta.Organizations[i]
		if org.Deleted {
			gs.organizations.Delete(org.ID)
			continue
		}
		gs.organizations.Store(org.ID, org)
	}

	for i := range delta.Budgets {
		budget := &delta.Budgets[i]
		if budget.Deleted {
			gs.DeleteBudget(ctx, budget.ID)
			continue
		}
		gs.UpsertBudgetConfig(ctx, budget.ID, budget)
	}

	for i := range delta.RateLimits {
		rl := &delta.RateLimits[i]
		if rl.Deleted {
			gs.DeleteRateLimit(ctx, rl.ID)
			continue
		}
		gs.UpsertRateLimitConfig(ctx, rl.ID, rl)
	}

	for i := range delta.ModelConfigs {
		mc := &delta.ModelConfigs[i]
		if mc.Deleted {
			gs.DeleteModelConfigInMemory(ctx, mc.ID)
			continue
		}
		gs.UpdateModelConfigInMemory(ctx, mc)
	}

	for i := range delta.Providers {
		provider := &delta.Providers[i]
		if provider.Deleted {
			gs.DeleteProviderInMemory(ctx, provider.Name)
			continue
		}
		gs.UpdateProviderInMemory(ctx, provider)
	}

	for i := range delta.VirtualKeys {
		vk := &delta.VirtualKeys[i]
		if vk.Deleted {
			gs.DeleteVirtualKeyInMemory(ctx, vk.ID)
			continue
		}
		gs.UpdateVirtualKeyInMemory(ctx, vk, nil, nil, nil)
	}

	for i := range delta.RoutingRules {
		_ = gs.UpdateRoutingRuleInMemory(ctx, &delta.RoutingRules[i])
	}
}

// loadFromConfigMemory loads all governance data from the config's memory into store's memory
func (gs *LocalGovernanceStore) loadFromConfigMemory(ctx context.Context, config *configstore.GovernanceConfig) error {
	if config == nil {
		return fmt.Errorf("governance config is nil")
	}

	organizations := config.Organizations
	budgets := config.Budgets
	virtualKeys := config.VirtualKeys
	rateLimits := config.RateLimits
	modelConfigs := config.ModelConfigs
	providers := config.Providers
	routingRules := config.RoutingRules

	// Hydrate parent entities from ownership columns on budget/rate limit rows.
	attachGovernanceFromReverseFK(budgets, rateLimits, providers, modelConfigs, virtualKeys)

	// Rebuild in-memory structures (lock-free)
	gs.rebuildInMemoryStructures(ctx, organizations, virtualKeys, budgets, rateLimits, modelConfigs, providers, routingRules)

	return nil
}

// rebuildInMemoryStructures rebuilds all in-memory data structures (lock-free)
func (gs *LocalGovernanceStore) rebuildInMemoryStructures(ctx context.Context, organizations []configstoreTables.TableOrganization, virtualKeys []configstoreTables.TableVirtualKey, budgets []configstoreTables.TableBudget, rateLimits []configstoreTables.TableRateLimit, modelConfigs []configstoreTables.TableModelConfig, providers []configstoreTables.TableProvider, routingRules []configstoreTables.TableRoutingRule) {
	// Clear existing data by creating new sync.Maps
	gs.virtualKeys = sync.Map{}
	gs.organizations = sync.Map{}
	gs.budgets = sync.Map{}
	gs.rateLimits = sync.Map{}
	gs.modelConfigs = sync.Map{}
	gs.providers = sync.Map{}
	gs.routingRules = sync.Map{}

	for i := range organizations {
		org := &organizations[i]
		gs.organizations.Store(org.ID, org)
	}

	attachGovernanceFromReverseFK(budgets, rateLimits, providers, modelConfigs, virtualKeys)

	// Build budgets map
	for i := range budgets {
		budget := &budgets[i]
		gs.budgets.Store(budget.ID, budget)
	}

	// Build rate limits map
	for i := range rateLimits {
		rateLimit := &rateLimits[i]
		gs.rateLimits.Store(rateLimit.ID, rateLimit)
	}

	// Build virtual keys map and track active VKs
	for i := range virtualKeys {
		vk := &virtualKeys[i]
		gs.storeVirtualKey(vk)
	}

	// Build model configs map
	// Key format: "modelName" for global configs, "modelName:provider" for provider-specific configs
	// Model names are normalized using GetBaseModelName to prevent duplicate config leakage
	// (e.g., "openai/gpt-4o" and "gpt-4o" both store under key "gpt-4o")
	for i := range modelConfigs {
		mc := &modelConfigs[i]
		if mc.Provider != nil {
			// Store under provider-specific key
			key := fmt.Sprintf("%s:%s", mc.ModelName, *mc.Provider)
			gs.modelConfigs.Store(key, mc)
		} else {
			// Global config (applies to all providers) - store under normalized model name
			key := mc.ModelName
			if gs.modelCatalog != nil {
				key = gs.modelCatalog.GetBaseModelName(mc.ModelName)
			}
			gs.modelConfigs.Store(key, mc)
		}
	}

	// Build providers map
	// Key format: provider name (e.g., "openai", "anthropic")
	for i := range providers {
		provider := &providers[i]
		gs.providers.Store(provider.Name, provider)
	}

	// Build routing rules map - O(n) single pass
	// Key format: "scope:scopeID" (scopeID empty string for global)
	rulesMap := make(map[string][]*configstoreTables.TableRoutingRule)

	for i := range routingRules {
		rule := &routingRules[i]
		rule.HydrateAssociationFromLegacy()
		key := rule.RoutingRulesCacheKey()

		// Group rules by key
		rulesMap[key] = append(rulesMap[key], rule)
	}

	// Sort each group by priority ASC (0 is highest priority, higher numbers are lower priority)
	for key, rules := range rulesMap {
		sort.Slice(rules, func(i, j int) bool {
			return rules[i].Priority < rules[j].Priority
		})
		gs.routingRules.Store(key, rules)
	}

	// Pre-compile all routing rule programs to avoid first-request latency
	gs.routingRules.Range(func(key, value interface{}) bool {
		if rules, ok := value.([]*configstoreTables.TableRoutingRule); ok {
			for _, rule := range rules {
				if _, err := gs.GetRoutingProgram(ctx, rule); err != nil {
					gs.logger.Warn("Failed to pre-compile routing program for rule %s: %v", rule.Name, err)
				}
			}
		}
		return true
	})

	// Load last DB usages from database entities (assign and populate inside mutexes to avoid race with ResetExpired*InMemory)
	gs.LastDBUsagesBudgetsMu.Lock()
	gs.LastDBUsagesBudgets = make(map[string]float64)
	for i := range budgets {
		budget := &budgets[i]
		gs.LastDBUsagesBudgets[budget.ID] = budget.CurrentUsage
	}
	gs.LastDBUsagesBudgetsMu.Unlock()

	gs.LastDBUsagesRateLimitsRequestsMu.Lock()
	gs.LastDBUsagesRateLimitsTokensMu.Lock()
	gs.LastDBUsagesRequestsRateLimits = make(map[string]int64)
	gs.LastDBUsagesTokensRateLimits = make(map[string]int64)
	for i := range rateLimits {
		rateLimit := &rateLimits[i]
		gs.LastDBUsagesRequestsRateLimits[rateLimit.ID] = rateLimit.RequestCurrentUsage
		gs.LastDBUsagesTokensRateLimits[rateLimit.ID] = rateLimit.TokenCurrentUsage
	}
	gs.LastDBUsagesRateLimitsTokensMu.Unlock()
	gs.LastDBUsagesRateLimitsRequestsMu.Unlock()
}

// collectRateLimitsFromHierarchy collects rate limits and their metadata from the hierarchy (Provider Configs → VK → Team → Customer)
func (gs *LocalGovernanceStore) collectRateLimitsFromHierarchy(ctx context.Context, vk *configstoreTables.TableVirtualKey, requestedProvider schemas.ModelProvider) map[string][]*configstoreTables.TableRateLimit {
	if vk == nil {
		return nil
	}

	rateLimitsWithCategories := map[string][]*configstoreTables.TableRateLimit{}
	seen := map[string]bool{}

	for _, pc := range vk.ProviderConfigs {
		if pc.Provider == string(requestedProvider) {
			appendLiveRateLimitsFromSlice(gs, rateLimitsWithCategories, pc.Provider, pc.RateLimits, seen)
		}
	}

	appendLiveRateLimitsFromSlice(gs, rateLimitsWithCategories, "VK", vk.RateLimits, seen)

	if vk.OrgID != nil {
		gs.appendOrgHierarchyRateLimits(*vk.OrgID, rateLimitsWithCategories, seen)
	}
	return rateLimitsWithCategories
}

// collectBudgetsFromHierarchy collects budgets and their metadata from the hierarchy (Provider Configs → VK → Customer -> User -> Team → BusinessUnit)
func (gs *LocalGovernanceStore) collectBudgetsFromHierarchy(_ context.Context, vk *configstoreTables.TableVirtualKey, requestedProvider schemas.ModelProvider) EntityWiseBudgets {
	if vk == nil {
		return nil
	}
	entityWiseBudgets := make(EntityWiseBudgets)
	// Collect all budgets in hierarchy order using lock-free sync.Map access (Provider Configs → VK → Team → Customer)
	seen := make(map[string]bool)
	for _, pc := range vk.ProviderConfigs {
		if pc.Provider != string(requestedProvider) {
			continue
		}
		// Multi-budgets
		for _, b := range pc.Budgets {
			if seen[b.ID] {
				continue
			}
			if budgetValue, exists := gs.budgets.Load(b.ID); exists && budgetValue != nil {
				if budget, ok := budgetValue.(*configstoreTables.TableBudget); ok && budget != nil {
					if categoryBudgets := entityWiseBudgets[pc.Provider]; categoryBudgets == nil {
						entityWiseBudgets[pc.Provider] = []*configstoreTables.TableBudget{}
					}
					entityWiseBudgets[pc.Provider] = append(entityWiseBudgets[pc.Provider], budget)
					seen[budget.ID] = true
				}
			}
		}
	}
	// VK-level multi-budgets
	for _, b := range vk.Budgets {
		if seen[b.ID] {
			continue
		}
		if budgetValue, exists := gs.budgets.Load(b.ID); exists && budgetValue != nil {
			if budget, ok := budgetValue.(*configstoreTables.TableBudget); ok && budget != nil {
				if categoryBudgets := entityWiseBudgets["VK"]; categoryBudgets == nil {
					entityWiseBudgets["VK"] = []*configstoreTables.TableBudget{}
				}
				entityWiseBudgets["VK"] = append(entityWiseBudgets["VK"], budget)
				seen[budget.ID] = true
			}
		}
	}
	if vk.OrgID != nil {
		gs.appendOrgHierarchyBudgets(*vk.OrgID, entityWiseBudgets, seen)
	}
	return entityWiseBudgets
}

// collectBudgetIDsFromMemory collects budget IDs from in-memory store data (lock-free)
func (gs *LocalGovernanceStore) collectBudgetIDsFromMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider) []string {
	budgetsWithCategory := gs.collectBudgetsFromHierarchy(ctx, vk, provider)
	budgetIDs := []string{}
	for _, budgets := range budgetsWithCategory {
		for _, budget := range budgets {
			budgetIDs = append(budgetIDs, budget.ID)
		}
	}
	return budgetIDs
}

// collectRateLimitIDsFromMemory collects rate limit IDs from in-memory store data (lock-free)
func (gs *LocalGovernanceStore) collectRateLimitIDsFromMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider) []string {
	rateLimitsWithCategories := gs.collectRateLimitsFromHierarchy(ctx, vk, provider)
	rateLimitIDs := []string{}
	for _, rateLimits := range rateLimitsWithCategories {
		for _, rateLimit := range rateLimits {
			rateLimitIDs = append(rateLimitIDs, rateLimit.ID)
		}
	}
	return rateLimitIDs
}

// CollectApplicableGovernanceIDs returns the budget and rate-limit IDs that are
// affected by a request with the given virtual key, provider, and model.
// It combines provider-level, model-level, and VK-hierarchy (team/customer) IDs.
// All lookups are fast in-memory sync.Map reads.
func (gs *LocalGovernanceStore) CollectApplicableGovernanceIDs(ctx context.Context, virtualKey string, provider schemas.ModelProvider, model string) (budgetIDs []string, rateLimitIDs []string) {
	seenBudgets := map[string]bool{}
	seenRateLimits := map[string]bool{}

	// --- Provider-level ---
	if provider != "" {
		providerKey := string(provider)
		if value, exists := gs.providers.Load(providerKey); exists && value != nil {
			if pt, ok := value.(*configstoreTables.TableProvider); ok && pt != nil {
				budgetIDs = appendBudgetIDs(budgetIDs, seenBudgets, pt.Budgets)
				rateLimitIDs = appendRateLimitIDs(rateLimitIDs, seenRateLimits, pt.RateLimits)
			}
		}
	}

	// --- Model-level ---
	if model != "" {
		// model+provider specific config
		if provider != "" {
			key := fmt.Sprintf("%s:%s", model, string(provider))
			if value, exists := gs.modelConfigs.Load(key); exists && value != nil {
				if mc, ok := value.(*configstoreTables.TableModelConfig); ok && mc != nil {
					budgetIDs = appendBudgetIDs(budgetIDs, seenBudgets, mc.Budgets)
					rateLimitIDs = appendRateLimitIDs(rateLimitIDs, seenRateLimits, mc.RateLimits)
				}
			}
		}
		// model-only config
		if mc, _ := gs.findModelOnlyConfig(ctx, model); mc != nil {
			budgetIDs = appendBudgetIDs(budgetIDs, seenBudgets, mc.Budgets)
			rateLimitIDs = appendRateLimitIDs(rateLimitIDs, seenRateLimits, mc.RateLimits)
		}
	}

	// --- VK hierarchy (provider-config → VK → team → customer) ---
	if virtualKey != "" {
		if vk, exists := gs.GetVirtualKey(ctx, virtualKey); exists && vk != nil {
			for _, id := range gs.collectBudgetIDsFromMemory(ctx, vk, provider) {
				if !seenBudgets[id] {
					budgetIDs = append(budgetIDs, id)
					seenBudgets[id] = true
				}
			}
			for _, id := range gs.collectRateLimitIDsFromMemory(ctx, vk, provider) {
				if !seenRateLimits[id] {
					rateLimitIDs = append(rateLimitIDs, id)
					seenRateLimits[id] = true
				}
			}
		}
	}

	return budgetIDs, rateLimitIDs
}

// PUBLIC API METHODS

// CreateVirtualKeyInMemory adds a new virtual key to the in-memory store (lock-free)
func (gs *LocalGovernanceStore) CreateVirtualKeyInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey) {
	if vk == nil {
		return // Nothing to create
	}

	// Store budgets
	for i := range vk.Budgets {
		vk.Budgets[i].IsCalendarAligned = vk.CalendarAligned
		gs.budgets.Store(vk.Budgets[i].ID, &vk.Budgets[i])
	}

	// Create associated rate limits
	for i := range vk.RateLimits {
		vk.RateLimits[i].IsCalendarAligned = vk.CalendarAligned
		gs.rateLimits.Store(vk.RateLimits[i].ID, &vk.RateLimits[i])
	}

	// Create provider config budgets and rate limits if they exist
	if vk.ProviderConfigs != nil {
		for i := range vk.ProviderConfigs {
			pc := &vk.ProviderConfigs[i]
			for j := range pc.Budgets {
				pc.Budgets[j].IsCalendarAligned = vk.CalendarAligned
				gs.budgets.Store(pc.Budgets[j].ID, &pc.Budgets[j])
			}
			for j := range pc.RateLimits {
				pc.RateLimits[j].IsCalendarAligned = vk.CalendarAligned
				gs.rateLimits.Store(pc.RateLimits[j].ID, &pc.RateLimits[j])
			}
		}
	}

	gs.storeVirtualKey(vk)
}

// UpdateVirtualKeyInMemory updates an existing virtual key in the in-memory store (lock-free)
func (gs *LocalGovernanceStore) UpdateVirtualKeyInMemory(ctx context.Context, vk *configstoreTables.TableVirtualKey, budgetBaselines map[string]float64, rateLimitTokensBaselines map[string]int64, rateLimitRequestsBaselines map[string]int64) {
	if vk == nil {
		return // Nothing to update
	}

	// Do not update the current usage of the rate limit, as it will be updated by the usage tracker.
	// But update if max limit or reset duration changes.
	existingVKValue, exists := gs.virtualKeys.Load(vk.ID)
	if exists && existingVKValue != nil {
		existingVK, ok := existingVKValue.(*configstoreTables.TableVirtualKey)
		if !ok || existingVK == nil {
			return // Nothing to update
		}

		// Create clone to avoid modifying the original
		clone := *vk

		// Collect all incoming budget IDs across VK + provider configs to avoid
		// deleting a budget that was moved between VK-level and PC-level in one update.
		allNewBudgetIDs := make(map[string]bool)
		for i := range clone.Budgets {
			allNewBudgetIDs[clone.Budgets[i].ID] = true
		}
		for i := range clone.ProviderConfigs {
			for j := range clone.ProviderConfigs[i].Budgets {
				allNewBudgetIDs[clone.ProviderConfigs[i].Budgets[j].ID] = true
			}
		}

		// Update multi-budgets for VK
		for i := range clone.Budgets {
			// Preserve existing usage from memory
			if existingBudgetValue, exists := gs.budgets.Load(clone.Budgets[i].ID); exists && existingBudgetValue != nil {
				if existingBudget, ok := existingBudgetValue.(*configstoreTables.TableBudget); ok && existingBudget != nil {
					clone.Budgets[i].CurrentUsage = existingBudget.CurrentUsage
					clone.Budgets[i].LastReset = existingBudget.LastReset
				}
			}
			clone.Budgets[i].IsCalendarAligned = clone.CalendarAligned
			gs.budgets.Store(clone.Budgets[i].ID, &clone.Budgets[i])
		}
		// Delete removed multi-budgets
		for _, oldBudget := range existingVK.Budgets {
			if !allNewBudgetIDs[oldBudget.ID] {
				gs.DeleteBudget(ctx, oldBudget.ID)
			}
		}

		allNewRateLimitIDs := make(map[string]bool)
		for i := range clone.RateLimits {
			allNewRateLimitIDs[clone.RateLimits[i].ID] = true
		}
		for i := range clone.ProviderConfigs {
			for j := range clone.ProviderConfigs[i].RateLimits {
				allNewRateLimitIDs[clone.ProviderConfigs[i].RateLimits[j].ID] = true
			}
		}

		for i := range clone.RateLimits {
			if existingRateLimitValue, exists := gs.rateLimits.Load(clone.RateLimits[i].ID); exists && existingRateLimitValue != nil {
				if existingRateLimit, ok := existingRateLimitValue.(*configstoreTables.TableRateLimit); ok && existingRateLimit != nil {
					clone.RateLimits[i].TokenCurrentUsage = existingRateLimit.TokenCurrentUsage
					clone.RateLimits[i].RequestCurrentUsage = existingRateLimit.RequestCurrentUsage
					clone.RateLimits[i].TokenLastReset = existingRateLimit.TokenLastReset
					clone.RateLimits[i].RequestLastReset = existingRateLimit.RequestLastReset
				}
			}
			clone.RateLimits[i].IsCalendarAligned = clone.CalendarAligned
			gs.rateLimits.Store(clone.RateLimits[i].ID, &clone.RateLimits[i])
		}
		for _, oldRL := range existingVK.RateLimits {
			if !allNewRateLimitIDs[oldRL.ID] {
				gs.DeleteRateLimit(ctx, oldRL.ID)
			}
		}

		if clone.ProviderConfigs != nil {
			// Create a map of existing provider configs by ID for fast lookup
			existingProviderConfigs := make(map[string]configstoreTables.TableVirtualKeyProviderConfig)
			if existingVK.ProviderConfigs != nil {
				for _, existingPC := range existingVK.ProviderConfigs {
					existingProviderConfigs[existingPC.ID] = existingPC
				}
			}

			// Process each new/updated provider config
			for i := range clone.ProviderConfigs {
				for j := range clone.ProviderConfigs[i].RateLimits {
					rl := &clone.ProviderConfigs[i].RateLimits[j]
					if existingRateLimitValue, exists := gs.rateLimits.Load(rl.ID); exists && existingRateLimitValue != nil {
						if existingRateLimit, ok := existingRateLimitValue.(*configstoreTables.TableRateLimit); ok && existingRateLimit != nil {
							rl.TokenCurrentUsage = existingRateLimit.TokenCurrentUsage
							rl.RequestCurrentUsage = existingRateLimit.RequestCurrentUsage
							rl.TokenLastReset = existingRateLimit.TokenLastReset
							rl.RequestLastReset = existingRateLimit.RequestLastReset
						}
					}
					rl.IsCalendarAligned = clone.CalendarAligned
					gs.rateLimits.Store(rl.ID, rl)
				}
				if existingPC, exists := existingProviderConfigs[clone.ProviderConfigs[i].ID]; exists {
					for _, oldRL := range existingPC.RateLimits {
						if !allNewRateLimitIDs[oldRL.ID] {
							gs.DeleteRateLimit(ctx, oldRL.ID)
						}
					}
				}
				// Update multi-budgets for provider config
				for j := range clone.ProviderConfigs[i].Budgets {
					b := &clone.ProviderConfigs[i].Budgets[j]
					if existingBudgetValue, exists := gs.budgets.Load(b.ID); exists && existingBudgetValue != nil {
						if existingBudget, ok := existingBudgetValue.(*configstoreTables.TableBudget); ok && existingBudget != nil {
							b.CurrentUsage = existingBudget.CurrentUsage
							b.LastReset = existingBudget.LastReset
						}
					}
					b.IsCalendarAligned = clone.CalendarAligned
					gs.budgets.Store(b.ID, b)
				}
				// Delete removed multi-budgets for this provider config
				if existingPC, exists := existingProviderConfigs[clone.ProviderConfigs[i].ID]; exists {
					for _, oldBudget := range existingPC.Budgets {
						if !allNewBudgetIDs[oldBudget.ID] {
							gs.DeleteBudget(ctx, oldBudget.ID)
						}
					}
				}
			}
			// Clean up orphaned rate limits and budgets from old provider configs
			// whose IDs changed (e.g., AP propagation replaces all configs with
			// new DB row IDs). Without this, stale entries leak memory and
			// pollute gossip baselines.
			for _, oldPC := range existingVK.ProviderConfigs {
				for _, oldRL := range oldPC.RateLimits {
					if !allNewRateLimitIDs[oldRL.ID] {
						gs.DeleteRateLimit(ctx, oldRL.ID)
					}
				}
				for _, oldBudget := range oldPC.Budgets {
					if !allNewBudgetIDs[oldBudget.ID] {
						gs.DeleteBudget(ctx, oldBudget.ID)
					}
				}
			}
		}
		gs.storeVirtualKey(&clone)
	} else {
		gs.CreateVirtualKeyInMemory(ctx, vk)
	}
}

// DeleteVirtualKeyInMemory removes a virtual key from the in-memory store
func (gs *LocalGovernanceStore) DeleteVirtualKeyInMemory(ctx context.Context, vkID string) {
	if vkID == "" {
		return // Nothing to delete
	}

	value, exists := gs.virtualKeys.Load(vkID)
	if !exists || value == nil {
		return
	}
	vk, ok := value.(*configstoreTables.TableVirtualKey)
	if !ok || vk == nil {
		gs.virtualKeys.Delete(vkID)
		return
	}

	for _, b := range vk.Budgets {
		gs.DeleteBudget(ctx, b.ID)
	}
	for _, rl := range vk.RateLimits {
		gs.DeleteRateLimit(ctx, rl.ID)
	}
	if vk.ProviderConfigs != nil {
		for _, pc := range vk.ProviderConfigs {
			for _, b := range pc.Budgets {
				gs.DeleteBudget(ctx, b.ID)
			}
			for _, rl := range pc.RateLimits {
				gs.DeleteRateLimit(ctx, rl.ID)
			}
		}
	}
	gs.virtualKeys.Delete(vkID)
}

// CollectOrgAncestorIDs returns orgID and each ancestor up to the root (child-first order).
func (gs *LocalGovernanceStore) CollectOrgAncestorIDs(orgID string) []string {
	if orgID == "" {
		return nil
	}
	ids := make([]string, 0, 4)
	gs.walkOrgAncestors(orgID, func(id string) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}

// GetUserGovernance retrieves user governance data by user ID (enterprise-only, lock-free)
func (gs *LocalGovernanceStore) GetUserGovernance(ctx context.Context, userID string) (*UserGovernance, bool) {
	// User governance is part of enterprise
	return nil, false
}

// CreateUserGovernanceInMemory adds user governance data to the in-memory store (enterprise-only)
func (gs *LocalGovernanceStore) CreateUserGovernanceInMemory(ctx context.Context, userID string, budget *configstoreTables.TableBudget, rateLimit *configstoreTables.TableRateLimit) {
	// NoOp
	// Available in enterprise
}

// UpdateUserGovernanceInMemory updates user governance data in the in-memory store (enterprise-only)
func (gs *LocalGovernanceStore) UpdateUserGovernanceInMemory(ctx context.Context, userID string, budget *configstoreTables.TableBudget, rateLimit *configstoreTables.TableRateLimit) {
	// NoOp
	// Available in enterprise
}

// DeleteUserGovernanceInMemory removes user governance data from the in-memory store (enterprise-only)
func (gs *LocalGovernanceStore) DeleteUserGovernanceInMemory(ctx context.Context, userID string) {
	// NoOp
	// Available in enterprise
}

// UpdateModelConfigInMemory adds or updates a model config in the in-memory store (lock-free)
// Preserves existing usage values when updating budgets and rate limits
// Returns the updated model config with potentially modified usage values
func (gs *LocalGovernanceStore) UpdateModelConfigInMemory(ctx context.Context, mc *configstoreTables.TableModelConfig) *configstoreTables.TableModelConfig {
	if mc == nil {
		return nil // Nothing to update
	}

	// Clone to avoid modifying the original
	clone := *mc

	for i := range clone.Budgets {
		if existingBudgetValue, exists := gs.budgets.Load(clone.Budgets[i].ID); exists && existingBudgetValue != nil {
			if eb, ok := existingBudgetValue.(*configstoreTables.TableBudget); ok && eb != nil {
				clone.Budgets[i].CurrentUsage = eb.CurrentUsage
			}
		}
		gs.budgets.Store(clone.Budgets[i].ID, &clone.Budgets[i])
	}

	for i := range clone.RateLimits {
		if existingRateLimitValue, exists := gs.rateLimits.Load(clone.RateLimits[i].ID); exists && existingRateLimitValue != nil {
			if erl, ok := existingRateLimitValue.(*configstoreTables.TableRateLimit); ok && erl != nil {
				clone.RateLimits[i].TokenCurrentUsage = erl.TokenCurrentUsage
				clone.RateLimits[i].RequestCurrentUsage = erl.RequestCurrentUsage
			}
		}
		gs.rateLimits.Store(clone.RateLimits[i].ID, &clone.RateLimits[i])
	}
	// Key format: "modelName" for global configs, "modelName:provider" for provider-specific configs
	if clone.Provider != nil {
		key := fmt.Sprintf("%s:%s", clone.ModelName, *clone.Provider)
		gs.modelConfigs.Store(key, &clone)
	} else {
		key := clone.ModelName
		if gs.modelCatalog != nil {
			key = gs.modelCatalog.GetBaseModelName(clone.ModelName)
		}
		gs.modelConfigs.Store(key, &clone)
	}

	return &clone
}

// DeleteModelConfigInMemory removes a model config from the in-memory store (lock-free)
func (gs *LocalGovernanceStore) DeleteModelConfigInMemory(ctx context.Context, mcID string) {
	if mcID == "" {
		return // Nothing to delete
	}

	// Find and delete the model config by ID
	gs.modelConfigs.Range(func(key, value interface{}) bool {
		mc, ok := value.(*configstoreTables.TableModelConfig)
		if !ok || mc == nil {
			return true // continue iteration
		}

		if mc.ID == mcID {
			for _, b := range mc.Budgets {
				gs.DeleteBudget(ctx, b.ID)
			}
			for _, rl := range mc.RateLimits {
				gs.DeleteRateLimit(ctx, rl.ID)
			}

			gs.modelConfigs.Delete(key)
			return false // stop iteration
		}
		return true // continue iteration
	})
}

// UpdateProviderInMemory adds or updates a provider in the in-memory store (lock-free)
// Preserves existing usage values when updating budgets and rate limits
// Returns the updated provider with potentially modified usage values
func (gs *LocalGovernanceStore) UpdateProviderInMemory(ctx context.Context, provider *configstoreTables.TableProvider) *configstoreTables.TableProvider {
	if provider == nil {
		return nil // Nothing to update
	}

	// Clone to avoid modifying the original
	clone := *provider

	for i := range clone.Budgets {
		if existingBudgetValue, exists := gs.budgets.Load(clone.Budgets[i].ID); exists && existingBudgetValue != nil {
			if eb, ok := existingBudgetValue.(*configstoreTables.TableBudget); ok && eb != nil {
				clone.Budgets[i].CurrentUsage = eb.CurrentUsage
			}
		}
		gs.budgets.Store(clone.Budgets[i].ID, &clone.Budgets[i])
	}

	for i := range clone.RateLimits {
		if existingRateLimitValue, exists := gs.rateLimits.Load(clone.RateLimits[i].ID); exists && existingRateLimitValue != nil {
			if erl, ok := existingRateLimitValue.(*configstoreTables.TableRateLimit); ok && erl != nil {
				clone.RateLimits[i].TokenCurrentUsage = erl.TokenCurrentUsage
				clone.RateLimits[i].RequestCurrentUsage = erl.RequestCurrentUsage
			}
		}
		gs.rateLimits.Store(clone.RateLimits[i].ID, &clone.RateLimits[i])
	}

	// Store under provider name
	gs.providers.Store(clone.Name, &clone)

	return &clone
}

// DeleteProviderInMemory removes a provider from the in-memory store (lock-free)
func (gs *LocalGovernanceStore) DeleteProviderInMemory(ctx context.Context, providerName string) {
	if providerName == "" {
		return // Nothing to delete
	}
	// Get provider to check for associated budget/rate limit
	if providerValue, exists := gs.providers.Load(providerName); exists && providerValue != nil {
		if provider, ok := providerValue.(*configstoreTables.TableProvider); ok && provider != nil {
			for _, b := range provider.Budgets {
				gs.DeleteBudget(ctx, b.ID)
			}
			for _, rl := range provider.RateLimits {
				gs.DeleteRateLimit(ctx, rl.ID)
			}
		}
	}
	gs.providers.Delete(providerName)
}

// Helper functions

// updateBudgetReferences updates all VKs, teams, customers, and provider configs that reference a reset budget
func (gs *LocalGovernanceStore) updateBudgetReferences(ctx context.Context, resetBudget *configstoreTables.TableBudget) {
	budgetID := resetBudget.ID
	// Update VKs that reference this budget
	gs.virtualKeys.Range(func(key, value interface{}) bool {
		vk, ok := value.(*configstoreTables.TableVirtualKey)
		if !ok || vk == nil {
			return true // continue
		}
		needsUpdate := false
		clone := *vk

		// Check VK-level budgets
		for i, b := range clone.Budgets {
			if b.ID == budgetID {
				clone.Budgets[i] = *resetBudget
				needsUpdate = true
			}
		}
		// Check provider config budgets
		if vk.ProviderConfigs != nil {
			for i := range clone.ProviderConfigs {
				for j, b := range clone.ProviderConfigs[i].Budgets {
					if b.ID == budgetID {
						clone.ProviderConfigs[i].Budgets[j] = *resetBudget
						needsUpdate = true
					}
				}
			}
		}
		if needsUpdate {
			gs.virtualKeys.Store(key, &clone)
		}
		return true // continue
	})
}

// updateRateLimitReferences updates all VKs, teams, customers, users and provider configs that reference a reset rate limit
func (gs *LocalGovernanceStore) updateRateLimitReferences(ctx context.Context, resetRateLimit *configstoreTables.TableRateLimit) {
	resetRateLimitID := resetRateLimit.ID
	// Update VKs that reference this rate limit
	gs.virtualKeys.Range(func(key, value interface{}) bool {
		vk, ok := value.(*configstoreTables.TableVirtualKey)
		if !ok || vk == nil {
			return true // continue
		}
		needsUpdate := false
		clone := *vk

		// Check VK-level rate limits
		for i, rl := range clone.RateLimits {
			if rl.ID == resetRateLimitID {
				clone.RateLimits[i] = *resetRateLimit
				needsUpdate = true
			}
		}

		// Check provider config rate limits
		if vk.ProviderConfigs != nil {
			for i := range clone.ProviderConfigs {
				for j, rl := range clone.ProviderConfigs[i].RateLimits {
					if rl.ID == resetRateLimitID {
						clone.ProviderConfigs[i].RateLimits[j] = *resetRateLimit
						needsUpdate = true
					}
				}
			}
		}

		if needsUpdate {
			gs.virtualKeys.Store(key, &clone)
		}
		return true // continue
	})
}

// HasRoutingRules checks if there are any routing rules configured
// Quick check to determine if we need to run routing evaluation at all
func (gs *LocalGovernanceStore) HasRoutingRules(ctx context.Context) bool {
	hasAny := false
	gs.routingRules.Range(func(_, _ interface{}) bool {
		hasAny = true
		return false // stop after first entry
	})
	return hasAny
}

// GetAllRoutingRules gets all routing rules from in-memory cache
func (gs *LocalGovernanceStore) GetAllRoutingRules(ctx context.Context) []*configstoreTables.TableRoutingRule {
	var result []*configstoreTables.TableRoutingRule

	// Iterate through all cached rules
	gs.routingRules.Range(func(_, value interface{}) bool {
		rules, ok := value.([]*configstoreTables.TableRoutingRule)
		if !ok {
			return true
		}
		result = append(result, rules...)
		return true
	})

	// Sort by priority ASC (0 is highest priority, higher numbers are lower priority), then created_at ASC
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})

	return result
}

// GetScopedRoutingRules retrieves routing rules by scope and scope ID (from in-memory cache)
// Rules are already sorted by priority ASC (0 is highest priority)
func (gs *LocalGovernanceStore) GetScopedRoutingRules(ctx context.Context, scope string, scopeID string) []*configstoreTables.TableRoutingRule {
	// Build cache key: "scope:scopeID" (scopeID empty string for global)
	var key string
	if scope == "global" {
		key = "global:"
	} else {
		key = fmt.Sprintf("%s:%s", scope, scopeID)
	}

	// Load from in-memory sync.Map
	rules, ok := gs.routingRules.Load(key)
	if !ok {
		return nil
	}

	rulesList, ok := rules.([]*configstoreTables.TableRoutingRule)
	if !ok {
		return nil
	}

	// Filter by enabled and return
	var enabledRules []*configstoreTables.TableRoutingRule
	for _, rule := range rulesList {
		if rule.EnabledValue() {
			enabledRules = append(enabledRules, rule)
		}
	}

	return enabledRules
}

// GetRoutingProgram compiles a CEL expression and caches the resulting program
// Uses the singleton CEL environment for efficiency
// Returns error if compilation fails
func (gs *LocalGovernanceStore) GetRoutingProgram(ctx context.Context, rule *configstoreTables.TableRoutingRule) (cel.Program, error) {
	if rule == nil {
		return nil, fmt.Errorf("routing rule cannot be nil")
	}

	// Check cache first to avoid recompilation
	if prog, ok := gs.compiledRoutingPrograms.Load(rule.ID); ok {
		if celProg, ok := prog.(cel.Program); ok {
			return celProg, nil
		}
	}

	// Get CEL expression, default to "true" if empty
	expr := rule.CelExpression
	if expr == "" {
		expr = "true"
	}

	// Normalize header and param keys to lowercase so CEL expressions match normalized map keys
	expr = routing.NormalizeMapKeysInCEL(expr)

	// Validate expression format
	if err := routing.ValidateCELExpression(expr); err != nil {
		return nil, fmt.Errorf("invalid CEL expression: %w", err)
	}

	// Compile using singleton environment
	ast, issues := gs.routingCELEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compile error: %s", issues.Err().Error())
	}

	// Create program
	program, err := gs.routingCELEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL program creation error: %w", err)
	}

	// Cache the compiled program
	gs.compiledRoutingPrograms.Store(rule.ID, program)

	return program, nil
}

// GetBudgetAndRateLimitStatus returns the current budget and rate limit status for provider and model combination
// Accounts for baseline usage from remote nodes when calculating percentages
func (gs *LocalGovernanceStore) GetBudgetAndRateLimitStatus(ctx context.Context, model string, provider schemas.ModelProvider, vk *configstoreTables.TableVirtualKey, budgetBaselines map[string]float64, tokenBaselines map[string]int64, requestBaselines map[string]int64) *BudgetAndRateLimitStatus {
	// Prevent nil pointer dereferences
	if budgetBaselines == nil {
		budgetBaselines = map[string]float64{}
	}
	if tokenBaselines == nil {
		tokenBaselines = map[string]int64{}
	}
	if requestBaselines == nil {
		requestBaselines = map[string]int64{}
	}

	result := &BudgetAndRateLimitStatus{
		BudgetPercentUsed:           0,
		RateLimitTokenPercentUsed:   0,
		RateLimitRequestPercentUsed: 0,
	}

	// Check model-specific rate limits and budgets (takes precedence)
	if model != "" {
		// Check model+provider config first (most specific)
		key := fmt.Sprintf("%s:%s", model, string(provider))
		if modelValue, ok := gs.modelConfigs.Load(key); ok && modelValue != nil {
			if modelConfig, ok := modelValue.(*configstoreTables.TableModelConfig); ok && modelConfig != nil {
				applyRateLimitStatusFromSlice(gs, modelConfig.RateLimits, tokenBaselines, requestBaselines, result)
				applyBudgetStatusFromSlice(gs, modelConfig.Budgets, budgetBaselines, result)
			}
		}

		// Fall back to model-only config (if exists)
		// Uses findModelOnlyConfig for cross-provider model name normalization
		if modelConfig, _ := gs.findModelOnlyConfig(ctx, model); modelConfig != nil {
			applyRateLimitStatusFromSlice(gs, modelConfig.RateLimits, tokenBaselines, requestBaselines, result)
			applyBudgetStatusFromSlice(gs, modelConfig.Budgets, budgetBaselines, result)
		}
	}

	// Check global provider-specific rate limits and budgets
	providerValue, ok := gs.providers.Load(string(provider))
	if ok && providerValue != nil {
		if providerTable, ok := providerValue.(*configstoreTables.TableProvider); ok && providerTable != nil {
			applyRateLimitStatusFromSlice(gs, providerTable.RateLimits, tokenBaselines, requestBaselines, result)
			applyBudgetStatusFromSlice(gs, providerTable.Budgets, budgetBaselines, result)
		}
	}

	// Check virtual key level provider-specific rate limits and budgets
	if vk != nil {
		if vk.ProviderConfigs != nil {
			for _, pc := range vk.ProviderConfigs {
				if pc.Provider == string(provider) {
					applyRateLimitStatusFromSlice(gs, pc.RateLimits, tokenBaselines, requestBaselines, result)
					applyBudgetStatusFromSlice(gs, pc.Budgets, budgetBaselines, result)
					break
				}
			}
		}
		applyRateLimitStatusFromSlice(gs, vk.RateLimits, tokenBaselines, requestBaselines, result)
		applyBudgetStatusFromSlice(gs, vk.Budgets, budgetBaselines, result)
	}
	return result
}

// UpdateRoutingRuleInMemory updates a routing rule in the in-memory cache
func (gs *LocalGovernanceStore) UpdateRoutingRuleInMemory(ctx context.Context, rule *configstoreTables.TableRoutingRule) error {
	if rule == nil {
		return fmt.Errorf("routing rule cannot be nil")
	}
	// First, remove the rule from ALL scopes (in case it was moved from one scope to another)
	gs.routingRules.Range(func(key, value interface{}) bool {
		rules, ok := value.([]*configstoreTables.TableRoutingRule)
		if !ok {
			return true
		}

		// Filter out the rule if it exists in this scope
		newRules := make([]*configstoreTables.TableRoutingRule, 0, len(rules))
		for _, r := range rules {
			if r.ID != rule.ID {
				newRules = append(newRules, r)
			}
		}

		// Update the scope with the filtered rules
		if len(newRules) != len(rules) {
			if len(newRules) == 0 {
				gs.routingRules.Delete(key)
			} else {
				gs.routingRules.Store(key, newRules)
			}
		}
		return true
	})
	// Build cache key for the new association
	key := rule.RoutingRulesCacheKey()
	// Load existing rules for this scope
	var rules []*configstoreTables.TableRoutingRule
	if value, ok := gs.routingRules.Load(key); ok {
		if existing, ok := value.([]*configstoreTables.TableRoutingRule); ok {
			rules = existing
		}
	}
	// Add the rule to the new scope
	rules = append(rules, rule)
	// Sort by priority ASC (0 is highest priority, higher numbers are lower priority)
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].Priority < rules[j].Priority
	})
	// Store back in cache
	gs.routingRules.Store(key, rules)
	// Invalidate compiled program cache for this rule (expression may have changed)
	gs.compiledRoutingPrograms.Delete(rule.ID)
	// Recompile the program immediately to update cache with fresh compilation
	if _, err := gs.GetRoutingProgram(ctx, rule); err != nil {
		gs.logger.Warn("Failed to recompile routing program for rule %s: %v", rule.Name, err)
	}
	return nil
}

// DeleteRoutingRuleInMemory removes a routing rule from the in-memory cache
func (gs *LocalGovernanceStore) DeleteRoutingRuleInMemory(ctx context.Context, id string) error {
	// Loop over all rules and delete the one with the matching id
	gs.routingRules.Range(func(key, value interface{}) bool {
		rules, ok := value.([]*configstoreTables.TableRoutingRule)
		if !ok {
			return true
		}
		// Find and filter out the rule with matching ID
		var filteredRules []*configstoreTables.TableRoutingRule
		for _, r := range rules {
			if r.ID != id {
				filteredRules = append(filteredRules, r)
			}
		}
		// Update or delete the key
		if len(filteredRules) == 0 {
			gs.routingRules.Delete(key)
		} else {
			gs.routingRules.Store(key, filteredRules)
		}
		return true
	})
	// Invalidate compiled program cache for this rule
	gs.compiledRoutingPrograms.Delete(id)
	return nil
}
