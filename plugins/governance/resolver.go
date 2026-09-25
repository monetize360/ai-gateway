// Package governance provides the budget evaluation and decision engine
package governance

import (
	"context"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

// Decision represents the result of governance evaluation
type Decision string

const (
	DecisionAllow              Decision = "allow"
	DecisionVirtualKeyNotFound Decision = "virtual_key_not_found"
	DecisionVirtualKeyBlocked  Decision = "virtual_key_blocked"
	DecisionRateLimited        Decision = "rate_limited"
	DecisionBudgetExceeded     Decision = "budget_exceeded"
	DecisionTokenLimited       Decision = "token_limited"
	DecisionRequestLimited     Decision = "request_limited"
	DecisionModelBlocked       Decision = "model_blocked"
	DecisionProviderBlocked    Decision = "provider_blocked"
	DecisionMCPToolBlocked     Decision = "mcp_tool_blocked"
	// DecisionMissingServiceID is returned when the requested model has no billing service
	// linked on config_models, so its spend cannot be attributed or limited.
	DecisionMissingServiceID Decision = "missing_service_id"
	// DecisionAccountAmbiguous is returned when an organization maps to more than one billing
	// account, making the account-scoped spend limits impossible to resolve.
	DecisionAccountAmbiguous Decision = "account_ambiguous"
	// DecisionWalletInsufficient is returned when a prepaid leaf account has no eligible
	// funded wallet, so the request cannot be billed from prepaid funds.
	DecisionWalletInsufficient Decision = "wallet_insufficient"
	// DecisionMissingAccountExternalID is returned when the org maps to exactly one billing
	// account but that account has no external_id, so usage cannot be published for rating.
	DecisionMissingAccountExternalID Decision = "missing_account_external_id"
)

// EvaluationRequest contains the context for evaluating a request
type EvaluationRequest struct {
	VirtualKey    string                `json:"virtual_key"` // Virtual key value
	Provider      schemas.ModelProvider `json:"provider"`
	Model         string                `json:"model"`
	UserID        string                `json:"user_id,omitempty"`         // Auth user ID (enterprise). Alone triggers VK budget skip when no tenant.
	BillingUserID string                `json:"billing_user_id,omitempty"` // Body user_id for budgetusage__m.user_id checks
}

// EvaluationResult contains the complete result of governance evaluation
type EvaluationResult struct {
	Decision      Decision                           `json:"decision"`
	Reason        string                             `json:"reason"`
	VirtualKey    *configstoreTables.TableVirtualKey `json:"virtual_key,omitempty"`
	RateLimitInfo *configstoreTables.TableRateLimit  `json:"rate_limit_info,omitempty"`
	BudgetInfo    []*configstoreTables.TableBudget   `json:"budget_info,omitempty"` // All budgets in hierarchy
	UsageInfo     *UsageInfo                         `json:"usage_info,omitempty"`
}

// UsageInfo represents current usage levels for rate limits and budgets
type UsageInfo struct {
	// Rate limit usage
	TokensUsedMinute   int64 `json:"tokens_used_minute"`
	TokensUsedHour     int64 `json:"tokens_used_hour"`
	TokensUsedDay      int64 `json:"tokens_used_day"`
	RequestsUsedMinute int64 `json:"requests_used_minute"`
	RequestsUsedHour   int64 `json:"requests_used_hour"`
	RequestsUsedDay    int64 `json:"requests_used_day"`

	// Budget usage
	VKBudgetUsage       int64 `json:"vk_budget_usage"`
	TeamBudgetUsage     int64 `json:"team_budget_usage"`
	CustomerBudgetUsage int64 `json:"customer_budget_usage"`
}

// BudgetResolver provides decision logic for the new hierarchical governance system
type BudgetResolver struct {
	store                   GovernanceStore
	logger                  schemas.Logger
	modelCatalog            *modelcatalog.ModelCatalog
	governanceInMemoryStore InMemoryStore
}

// NewBudgetResolver creates a new budget-based governance resolver
func NewBudgetResolver(store GovernanceStore, modelCatalog *modelcatalog.ModelCatalog, logger schemas.Logger, governanceInMemoryStore InMemoryStore) *BudgetResolver {
	return &BudgetResolver{
		store:                   store,
		logger:                  logger,
		modelCatalog:            modelCatalog,
		governanceInMemoryStore: governanceInMemoryStore,
	}
}

// EvaluateModelAndProviderRequest evaluates provider-level and model-level rate limits.
// This applies even when virtual keys are disabled or not present. Spend limits are not
// checked here — they live entirely on BudgetUsage (see EvaluateBudgetUsageRequest).
func (r *BudgetResolver) EvaluateModelAndProviderRequest(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string) *EvaluationResult {
	// Create evaluation request for the checks
	request := &EvaluationRequest{
		Provider: provider,
		Model:    model,
	}
	// 1. Check provider-level rate limits FIRST (before model-level checks)
	if provider != "" {
		if decision, err := r.store.CheckProviderRateLimit(ctx, request, nil, nil); err != nil || isRateLimitViolation(decision) {
			return &EvaluationResult{
				Decision: decision,
				Reason:   fmt.Sprintf("Provider-level rate limit check failed: %s", reasonFromErr(err, decision)),
			}
		}
	}
	// 2. Check model-level rate limits (after provider-level checks)
	if model != "" {
		if decision, err := r.store.CheckModelRateLimit(ctx, request, nil, nil); err != nil || isRateLimitViolation(decision) {
			return &EvaluationResult{
				Decision: decision,
				Reason:   fmt.Sprintf("Model-level rate limit check failed: %s", reasonFromErr(err, decision)),
			}
		}
	}
	// All provider-level and model-level checks passed
	return &EvaluationResult{
		Decision: DecisionAllow,
		Reason:   "Request allowed by governance policy (provider-level and model-level checks passed)",
	}
}

// EvaluateOrgHierarchyRequest evaluates org-scoped rate limits, walking org → parent → … → root.
func (r *BudgetResolver) EvaluateOrgHierarchyRequest(ctx *schemas.BifrostContext, orgID string, request *EvaluationRequest) *EvaluationResult {
	if orgID == "" {
		return &EvaluationResult{
			Decision: DecisionAllow,
			Reason:   "No org ID provided, skipping org-level checks",
		}
	}
	if decision, err := r.store.CheckOrgHierarchyRateLimit(ctx, orgID, request, nil, nil); err != nil || isRateLimitViolation(decision) {
		return &EvaluationResult{
			Decision: decision,
			Reason:   fmt.Sprintf("Org-level rate limit exceeded: %s", reasonFromErr(err, decision)),
		}
	}
	return &EvaluationResult{
		Decision: DecisionAllow,
		Reason:   "Org-level checks passed",
	}
}

// EvaluateBudgetUsageRequest enforces every spend limit that applies to a request, reading
// BudgetUsage (budgetusage__m) only. MPilot Rating owns the usage numbers; the gateway just
// compares them against their limits, in order of increasing scope:
//
//  1. the billing service behind the requested model (a model with no service cannot be rated),
//  2. the provider,
//  3. the billing users on the key and the request,
//  4. the org-unit chain of those users (independent of user-scoped BudgetUsage),
//  5. the billing account of the key's organization and each of its ancestors,
//  6. the leaf account must have external_id (rating's billingAccountRef),
//  7. a funded prepaid wallet on the leaf account (when that account is prepaid).
func (r *BudgetResolver) EvaluateBudgetUsageRequest(ctx *schemas.BifrostContext, request *EvaluationRequest, vk *configstoreTables.TableVirtualKey) *EvaluationResult {
	if request == nil {
		return &EvaluationResult{
			Decision: DecisionAllow,
			Reason:   "No evaluation request, skipping budget usage checks",
		}
	}

	// 1. Billing service behind the model.
	if model := strings.TrimSpace(request.Model); model != "" {
		serviceID := strings.TrimSpace(r.store.ResolveConfigModelServiceID(request.Provider, model))
		if serviceID == "" {
			return &EvaluationResult{
				Decision:   DecisionMissingServiceID,
				Reason:     fmt.Sprintf("Model '%s' is not mapped to a billing service", model),
				VirtualKey: vk,
			}
		}
		if decision, err := r.store.CheckServiceBudgetUsage(ctx, serviceID, request, nil); err != nil || isBudgetViolation(decision) {
			return &EvaluationResult{
				Decision:   decision,
				Reason:     fmt.Sprintf("Service-level budget exceeded: %s", reasonFromErr(err, decision)),
				VirtualKey: vk,
			}
		}
	}

	// 2. Provider.
	if providerID := strings.TrimSpace(r.store.ResolveConfigProviderID(request.Provider)); providerID != "" {
		if decision, err := r.store.CheckProviderBudgetUsage(ctx, providerID, request, nil); err != nil || isBudgetViolation(decision) {
			return &EvaluationResult{
				Decision:   decision,
				Reason:     fmt.Sprintf("Provider-level budget exceeded: %s", reasonFromErr(err, decision)),
				VirtualKey: vk,
			}
		}
	}

	// 3. Billing users: the key's own user plus any user carried on the request.
	virtualKeyUserID := ""
	if vk != nil && vk.UserID != nil {
		virtualKeyUserID = strings.TrimSpace(*vk.UserID)
	}
	for _, userID := range uniqueNonEmptyStrings(virtualKeyUserID, request.UserID, request.BillingUserID) {
		if decision, err := r.store.CheckUserBudgetUsage(ctx, userID, request, nil); err != nil || isBudgetViolation(decision) {
			return &EvaluationResult{
				Decision:   decision,
				Reason:     fmt.Sprintf("User-level budget exceeded: %s", reasonFromErr(err, decision)),
				VirtualKey: vk,
			}
		}
	}

	// 4. Org-unit hierarchy (independent of user-scoped BudgetUsage).
	orgUnitUserIDs := uniqueNonEmptyStrings(virtualKeyUserID, request.UserID, request.BillingUserID)
	if decision, err := r.store.CheckOrgUnitHierarchyBudgetUsage(ctx, orgUnitUserIDs, request, nil); err != nil || isBudgetViolation(decision) {
		return &EvaluationResult{
			Decision:   decision,
			Reason:     fmt.Sprintf("Org-unit budget exceeded: %s", reasonFromErr(err, decision)),
			VirtualKey: vk,
		}
	}

	// 5. Account hierarchy of the key's organization.
	if vk != nil {
		if orgID := vk.GovernanceScopeOrgIDString(); orgID != "" {
			decision, err := r.store.CheckAccountHierarchyBudgetUsage(ctx, orgID, request, nil)
			if decision == DecisionAccountAmbiguous {
				return &EvaluationResult{
					Decision:   decision,
					Reason:     fmt.Sprintf("Account hierarchy could not be resolved: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
			if err != nil || isBudgetViolation(decision) {
				return &EvaluationResult{
					Decision:   decision,
					Reason:     fmt.Sprintf("Account-level budget exceeded: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
		}
	}

	// 6. Leaf account external_id is required to publish usage for rating.
	if vk != nil {
		if orgID := vk.GovernanceScopeOrgIDString(); orgID != "" {
			decision, err := r.store.CheckLeafAccountExternalID(orgID)
			if decision == DecisionAccountAmbiguous {
				return &EvaluationResult{
					Decision:   decision,
					Reason:     fmt.Sprintf("Account hierarchy could not be resolved: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
			if decision == DecisionMissingAccountExternalID {
				return &EvaluationResult{
					Decision:   decision,
					Reason:     fmt.Sprintf("Billing account has no external_id: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
		}
	}

	// 7. Prepaid wallet on the leaf billing account.
	if vk != nil {
		if orgID := vk.GovernanceScopeOrgIDString(); orgID != "" {
			serviceID := strings.TrimSpace(r.store.ResolveConfigModelServiceID(request.Provider, request.Model))
			decision, err := r.store.CheckPrepaidWallet(ctx, orgID, serviceID)
			if decision == DecisionAccountAmbiguous {
				return &EvaluationResult{
					Decision:   decision,
					Reason:     fmt.Sprintf("Account hierarchy could not be resolved: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
			if err != nil || decision == DecisionWalletInsufficient {
				return &EvaluationResult{
					Decision:   DecisionWalletInsufficient,
					Reason:     fmt.Sprintf("Prepaid wallet check failed: %s", reasonFromErr(err, decision)),
					VirtualKey: vk,
				}
			}
		}
	}

	return &EvaluationResult{
		Decision:   DecisionAllow,
		Reason:     "Budget usage checks passed",
		VirtualKey: vk,
	}
}

// isModelRequired checks if the requested model is required for this request
func (r *BudgetResolver) isModelRequired(requestType schemas.RequestType) bool {
	// Here we will have to check for some requests which do not need model
	// For example, batches, container, files, videos, passthrough requests
	// For these requests, we will only check for provider filtering
	if requestType == schemas.ListModelsRequest || requestType == schemas.MCPToolExecutionRequest || requestType == schemas.BatchCreateRequest || requestType == schemas.BatchListRequest || requestType == schemas.BatchRetrieveRequest || requestType == schemas.BatchCancelRequest || requestType == schemas.BatchResultsRequest || requestType == schemas.FileUploadRequest || requestType == schemas.FileListRequest || requestType == schemas.FileRetrieveRequest || requestType == schemas.FileDeleteRequest || requestType == schemas.FileContentRequest || requestType == schemas.ContainerCreateRequest || requestType == schemas.ContainerListRequest || requestType == schemas.ContainerRetrieveRequest || requestType == schemas.ContainerDeleteRequest || requestType == schemas.ContainerFileCreateRequest || requestType == schemas.ContainerFileListRequest || requestType == schemas.ContainerFileRetrieveRequest || requestType == schemas.ContainerFileContentRequest || requestType == schemas.ContainerFileDeleteRequest || requestType == schemas.VideoRetrieveRequest || requestType == schemas.VideoDownloadRequest || requestType == schemas.VideoListRequest || requestType == schemas.VideoDeleteRequest || requestType == schemas.VideoRemixRequest || requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest {
		return false
	}
	return true
}

// EvaluateVirtualKeyRequest evaluates virtual key-specific checks: validation, provider/model
// filtering, and rate limits. Spend limits are not evaluated here — they live on BudgetUsage.
// skipRateLimits is true when rate limits must be bypassed (metadata calls, or user auth where
// user governance owns the limits).
func (r *BudgetResolver) EvaluateVirtualKeyRequest(ctx *schemas.BifrostContext, virtualKeyValue string, provider schemas.ModelProvider, model string, requestType schemas.RequestType, skipRateLimits bool) *EvaluationResult {
	// 1. Validate virtual key exists and is active
	vk, exists := r.store.GetVirtualKey(ctx, virtualKeyValue)
	if !exists {
		return &EvaluationResult{
			Decision: DecisionVirtualKeyNotFound,
			Reason:   "Virtual key not found",
		}
	}
	// Set virtual key id and name in context
	ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, vk.ID)
	ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyName, vk.Name)
	stampVirtualKeyOrgContext(ctx, vk)
	if !vk.IsActiveValue() {
		return &EvaluationResult{
			Decision: DecisionVirtualKeyBlocked,
			Reason:   "Virtual key is inactive",
		}
	}
	// 2a. Check provider access policy (org-level then VK-level allow/block rules)
	if requestType != schemas.MCPToolExecutionRequest {
		if allowed, reason := isProviderAllowedByAccessPolicy(vk, provider); !allowed {
			return &EvaluationResult{
				Decision:   DecisionProviderBlocked,
				Reason:     reason,
				VirtualKey: vk,
			}
		}
	}
	// 2b. Check provider filtering (legacy provider config check)
	if requestType != schemas.MCPToolExecutionRequest && !r.isProviderAllowed(vk, provider) {
		return &EvaluationResult{
			Decision:   DecisionProviderBlocked,
			Reason:     fmt.Sprintf("Provider '%s' is not allowed for this virtual key", provider),
			VirtualKey: vk,
		}
	}
	// 3. Check model filtering
	if r.isModelRequired(requestType) && !r.isModelAllowed(vk, provider, model) {
		return &EvaluationResult{
			Decision:   DecisionModelBlocked,
			Reason:     fmt.Sprintf("Model '%s' is not allowed for this virtual key", model),
			VirtualKey: vk,
		}
	}

	evaluationRequest := &EvaluationRequest{
		VirtualKey: virtualKeyValue,
		Provider:   provider,
		Model:      model,
	}

	// 4. Check rate limits hierarchy (VK level)
	if !skipRateLimits {
		if rateLimitResult := r.checkRateLimitHierarchy(ctx, vk, evaluationRequest); rateLimitResult != nil {
			return rateLimitResult
		}
	}

	// Find the provider config that matches the request's provider and apply key filtering
	for _, pc := range vk.AllowedModelConfigs {
		if schemas.ModelProvider(pc.Provider) == provider {
			if !pc.AllowAllKeys {
				// Restrict to specific keys (empty slice = no keys allowed)
				includeOnlyKeys := make([]string, 0, len(pc.Keys))
				for _, dbKey := range pc.Keys {
					includeOnlyKeys = append(includeOnlyKeys, dbKey.KeyID)
				}
				ctx.SetValue(schemas.BifrostContextKeyGovernanceIncludeOnlyKeys, includeOnlyKeys)
			}
			break
		}
	}

	// All checks passed
	return &EvaluationResult{
		Decision:   DecisionAllow,
		Reason:     "Request allowed by governance policy",
		VirtualKey: vk,
	}
}

// isProviderAndModelAccessible reports whether the VK may call provider/model under
// the same identity checks EvaluateVirtualKeyRequest applies (provider access policy,
// provider allowlist, and org+VK model allow/block). Budgets and rate limits are not
// considered — callers that need spend gates use the dedicated helper methods.
func (r *BudgetResolver) isProviderAndModelAccessible(vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, model string) bool {
	if r == nil || vk == nil {
		return false
	}
	if allowed, _ := isProviderAllowedByAccessPolicy(vk, provider); !allowed {
		return false
	}
	if !r.isProviderAllowed(vk, provider) {
		return false
	}
	return r.isModelAllowed(vk, provider, model)
}

// isModelAllowed checks if the requested model is allowed for this VK.
// Enforcement is layered: org-level configs are checked first, then VK-level configs.
// Within each layer blacklisted models win over allowed models (blacklist-wins semantics).
//
// Layer 1 — Org-level (OrgAllowedModelConfigs, populated from scope_org_id configs):
//   - If any org config blacklists the model → block immediately (highest priority).
//   - If org configs exist for this provider but none allow the model → block.
//   - If no org configs exist for this provider → layer passes (no org restriction).
//
// Layer 2 — VK-level (AllowedModelConfigs):
//   - Same blacklist-wins, then allowlist logic as before.
//   - Empty VK configs means no VK-level restriction (allow all from this layer).
func (r *BudgetResolver) isModelAllowed(vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider, model string) bool {
	// --- Layer 1: Org-level restrictions ---
	if len(vk.OrgAllowedModelConfigs) > 0 {
		orgHasConfigForProvider := false
		for _, pc := range vk.OrgAllowedModelConfigs {
			if pc.Provider == string(provider) {
				orgHasConfigForProvider = true
				// Blacklist wins immediately.
				if isModelBlockedByList(pc.BlacklistedModels, model) {
					return false
				}
			}
		}
		if orgHasConfigForProvider {
			// At least one org config exists for this provider; check allowlist.
			orgAllows := false
			for _, pc := range vk.OrgAllowedModelConfigs {
				if pc.Provider != string(provider) {
					continue
				}
				if r.modelCatalog != nil && r.governanceInMemoryStore != nil {
					providerConfig, ok := r.governanceInMemoryStore.GetConfiguredProviders()[provider]
					providerConfigPtr := &providerConfig
					if !ok {
						providerConfigPtr = nil
					}
					if r.modelCatalog.IsModelAllowedForProvider(provider, model, providerConfigPtr, pc.AllowedModels) {
						orgAllows = true
						break
					}
				} else if pc.AllowedModels.IsAllowed(model) {
					orgAllows = true
					break
				}
			}
			if !orgAllows {
				return false
			}
		}
	}

	// --- Layer 2: VK-level restrictions ---
	// Empty VK configs means no VK-level restriction.
	if len(vk.AllowedModelConfigs) == 0 {
		return true
	}

	// Pass 1: if any matching VK config blacklists the model, block immediately.
	for _, pc := range vk.AllowedModelConfigs {
		if pc.Provider == string(provider) && isModelBlockedByList(pc.BlacklistedModels, model) {
			return false
		}
	}

	// Pass 2: allowlist check — model is allowed if any matching VK config permits it.
	for _, pc := range vk.AllowedModelConfigs {
		if pc.Provider == string(provider) {
			if r.modelCatalog != nil && r.governanceInMemoryStore != nil {
				providerConfig, ok := r.governanceInMemoryStore.GetConfiguredProviders()[provider]
				providerConfigPtr := &providerConfig
				if !ok {
					providerConfigPtr = nil
				}
				if r.modelCatalog.IsModelAllowedForProvider(provider, model, providerConfigPtr, pc.AllowedModels) {
					return true
				}
			} else if pc.AllowedModels.IsAllowed(model) {
				return true
			}
		}
	}

	return false
}

// isProviderAllowed checks if the requested provider is allowed for this VK.
// Org-level configs restrict which providers are accessible, followed by VK-level configs.
// isProviderAllowedByAccessPolicy evaluates the provider access policy (the new
// governance_provider_access table) for a VK. It checks org-level first, then
// VK-level, with blacklist-wins semantics.
//
// Returns (true, "") when the provider is permitted, or (false, reason) when blocked.
func isProviderAllowedByAccessPolicy(vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider) (bool, string) {
	providerName := string(provider)

	// Layer 1: org-level provider access policy
	if vk.OrgProviderAccessPolicy != nil && !vk.OrgProviderAccessPolicy.IsEmpty() {
		p := vk.OrgProviderAccessPolicy
		if p.BlacklistedProviders.IsBlocked(providerName) {
			return false, fmt.Sprintf("Provider '%s' is blocked by organization policy", provider)
		}
		if len(p.AllowedProviders) > 0 && !p.AllowedProviders.IsAllowed(providerName) {
			return false, fmt.Sprintf("Provider '%s' is not in the organization allow list", provider)
		}
	}

	// Layer 2: VK-level provider access policy
	if vk.ProviderAccessPolicy != nil && !vk.ProviderAccessPolicy.IsEmpty() {
		p := vk.ProviderAccessPolicy
		if p.BlacklistedProviders.IsBlocked(providerName) {
			return false, fmt.Sprintf("Provider '%s' is blocked for this virtual key", provider)
		}
		if len(p.AllowedProviders) > 0 && !p.AllowedProviders.IsAllowed(providerName) {
			return false, fmt.Sprintf("Provider '%s' is not in the virtual key allow list", provider)
		}
	}

	return true, ""
}

func (r *BudgetResolver) isProviderAllowed(vk *configstoreTables.TableVirtualKey, provider schemas.ModelProvider) bool {
	// Org-level: if org has configs and none match this provider, block it.
	if len(vk.OrgAllowedModelConfigs) > 0 {
		orgPermitsProvider := false
		for _, pc := range vk.OrgAllowedModelConfigs {
			if pc.Provider == string(provider) {
				orgPermitsProvider = true
				break
			}
		}
		if !orgPermitsProvider {
			return false
		}
	}

	// VK-level: empty means no restriction (all providers allowed at this layer).
	if len(vk.AllowedModelConfigs) == 0 {
		return true
	}

	for _, pc := range vk.AllowedModelConfigs {
		if pc.Provider == string(provider) {
			return true
		}
	}

	return false
}

// checkRateLimitHierarchy checks provider-level rate limits first, then VK rate limits using flexible approach
func (r *BudgetResolver) checkRateLimitHierarchy(ctx context.Context, vk *configstoreTables.TableVirtualKey, request *EvaluationRequest) *EvaluationResult {
	if decision, err := r.store.CheckVirtualKeyRateLimit(ctx, vk, request, nil, nil); err != nil || isRateLimitViolation(decision) {
		// Check provider-level first (matching check order), then VK-level
		var rateLimitInfo *configstoreTables.TableRateLimit
		for _, pc := range vk.AllowedModelConfigs {
			if pc.Provider == string(request.Provider) && len(pc.RateLimits) > 0 {
				rateLimitInfo = &pc.RateLimits[0]
				break
			}
		}
		if rateLimitInfo == nil && len(vk.RateLimits) > 0 {
			rateLimitInfo = &vk.RateLimits[0]
		}
		return &EvaluationResult{
			Decision:      decision,
			Reason:        fmt.Sprintf("Rate limit check failed: %s", reasonFromErr(err, decision)),
			VirtualKey:    vk,
			RateLimitInfo: rateLimitInfo,
		}
	}

	return nil // No rate limit violations
}

// Helper methods for provider config validation (used by TransportInterceptor)

// isProviderBudgetViolated checks if a provider config's budget is violated
func (r *BudgetResolver) isProviderBudgetViolated(ctx context.Context, vk *configstoreTables.TableVirtualKey, config configstoreTables.TableAllowedModelConfig) bool {
	request := &EvaluationRequest{Provider: schemas.ModelProvider(config.Provider)}

	// 1. Check global provider-level budget first
	if _, err := r.store.CheckProviderBudget(ctx, request, nil); err != nil {
		r.logger.Debug(fmt.Sprintf("Global provider budget exceeded for provider %s: %s", config.Provider, err.Error()))
		return true
	}

	// 2. Check VK-level provider config budget
	if len(config.Budgets) == 0 {
		return false
	}
	if _, err := r.store.CheckVirtualKeyBudget(ctx, vk, request, nil); err != nil {
		r.logger.Debug(fmt.Sprintf("VK provider config budget exceeded for VK %s: %s", vk.ID, err.Error()))
		return true
	}
	return false
}

// isProviderRateLimitViolated checks if a provider config's rate limit is violated
func (r *BudgetResolver) isProviderRateLimitViolated(ctx context.Context, vk *configstoreTables.TableVirtualKey, config configstoreTables.TableAllowedModelConfig) bool {
	request := &EvaluationRequest{Provider: schemas.ModelProvider(config.Provider)}

	// 1. Check global provider-level rate limit first
	if decision, err := r.store.CheckProviderRateLimit(ctx, request, nil, nil); err != nil || isRateLimitViolation(decision) {
		r.logger.Debug(fmt.Sprintf("Global provider rate limit exceeded for provider %s", config.Provider))
		return true
	}

	// 2. Check VK-level provider config rate limit
	if len(config.RateLimits) == 0 {
		return false
	}
	decision, err := r.store.CheckVirtualKeyRateLimit(ctx, vk, request, nil, nil)
	if err != nil || isRateLimitViolation(decision) {
		r.logger.Debug(fmt.Sprintf("VK provider config rate limit exceeded for VK %s, provider %s", vk.ID, config.Provider))
		return true
	}
	return false
}

// isRateLimitViolation returns true if the decision indicates a rate limit violation
func isRateLimitViolation(decision Decision) bool {
	return decision == DecisionRateLimited || decision == DecisionTokenLimited || decision == DecisionRequestLimited
}

// isBudgetViolation returns true if the decision indicates a budget violation.
func isBudgetViolation(decision Decision) bool {
	return decision == DecisionBudgetExceeded
}

// reasonFromErr yields a non-nil-safe reason string. When the store returns a
// non-allow decision without an accompanying error, err.Error() would panic —
// fall back to a generic phrase that still names the decision.
func reasonFromErr(err error, decision Decision) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("policy violation (%s)", decision)
}
