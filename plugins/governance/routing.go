package governance

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// DefaultRoutingChainMaxDepth is the default maximum depth for routing rule chain evaluation.
const DefaultRoutingChainMaxDepth = 10

// ScopeLevel represents a level in the scope precedence hierarchy
type ScopeLevel struct {
	ScopeName string // "virtual_key", "org", or "global"
	ScopeID   string // empty string for global scope
}

// RoutingDecision is the output of routing rule evaluation
// Represents which provider/model to route to and fallback chain
type RoutingDecision struct {
	Provider        string   // Primary provider (e.g., "openai", "azure")
	Model           string   // Model to use (or empty to use original)
	KeyID           string   // Optional: pin a specific API key by UUID ("" = no pin)
	Fallbacks       []string // Fallback chain: ["provider/model", ...]
	MatchedRuleID   string   // ID of the rule that matched
	MatchedRuleName string   // Name of the rule that matched
}

// RoutingContext holds all data needed for routing rule evaluation
// Reuses existing configstore table types for VirtualKey, Team, Customer
type RoutingContext struct {
	VirtualKey               *configstoreTables.TableVirtualKey // nil if no VK
	Provider                 schemas.ModelProvider              // Current provider
	Model                    string                             // Current model
	RequestType              string                             // Normalized request type (e.g., "chat_completion", "embedding") from HTTP context
	Fallbacks                []string                           // Fallback chain: ["provider/model", ...]
	Headers                  map[string]string                  // Request headers for dynamic routing
	QueryParams              map[string]string                  // Query parameters for dynamic routing
	BudgetAndRateLimitStatus *BudgetAndRateLimitStatus          // Budget and rate limit status by provider/model
}

type RoutingEngine struct {
	store         GovernanceStore
	logger        schemas.Logger
	chainMaxDepth *int // pointer to live config value; changes are reflected immediately
}

// NewRoutingEngine creates a new RoutingEngine
func NewRoutingEngine(store GovernanceStore, logger schemas.Logger, chainMaxDepth *int) (*RoutingEngine, error) {
	if store == nil {
		return nil, fmt.Errorf("store cannot be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if chainMaxDepth == nil {
		return nil, fmt.Errorf("chainMaxDepth cannot be nil")
	}
	if *chainMaxDepth <= 0 {
		return nil, fmt.Errorf("chainMaxDepth must be greater than 0")
	}

	return &RoutingEngine{
		store:         store,
		logger:        logger,
		chainMaxDepth: chainMaxDepth,
	}, nil
}

// EvaluateRoutingRules evaluates routing rules for a given context and returns a routing decision.
// Implements scope precedence: VirtualKey > Team > Customer > Global (first-match-wins within each iteration).
// When a matched rule has chain_rule=true, the resolved provider/model is fed back into the evaluator
// and the full scope chain is re-evaluated with the updated context. This repeats until:
//  1. No rule matches the current context
//  2. A terminal rule matches (chain_rule=false, the default)
//  3. Every chain-rule that could match has already fired once (all candidates exhausted)
//  4. The chain exceeds the configured max depth (chainMaxDepth, default 10)
func (re *RoutingEngine) EvaluateRoutingRules(ctx *schemas.BifrostContext, routingCtx *RoutingContext) (*RoutingDecision, error) {
	if routingCtx == nil {
		return nil, fmt.Errorf("routing context cannot be nil")
	}

	re.logger.Debug("[RoutingEngine] Starting rule evaluation for provider=%s, model=%s", routingCtx.Provider, routingCtx.Model)

	// Mutable provider/model that advances through the chain; all other context fields are immutable.
	currentProvider := routingCtx.Provider
	currentModel := routingCtx.Model

	// Track which rule IDs have already fired to prevent a rule from matching more than once per chain.
	// This allows a self-looping rule (target == current state) to fire once and then let subsequent
	// rules in the chain run, rather than halting with a cycle error.
	visitedRuleIDs := map[string]struct{}{}

	// Build scope chain once — it's based on the immutable VirtualKey and won't change across chain steps.
	var orgAncestors []string
	if routingCtx.VirtualKey != nil {
		if scopeOrgID := routingCtx.VirtualKey.GovernanceScopeOrgID(); scopeOrgID != nil {
			orgAncestors = re.store.CollectOrgAncestorIDs(*scopeOrgID)
		}
	}
	scopeChain := buildScopeChain(routingCtx.VirtualKey, orgAncestors)

	// Cache rules per scope upfront to avoid redundant store lookups when rules chain
	// and we re-evaluate the scope hierarchy on subsequent steps.
	rulesPerScope := make(map[ScopeLevel][]*configstoreTables.TableRoutingRule, len(scopeChain))
	for _, scope := range scopeChain {
		rules := re.store.GetScopedRoutingRules(ctx, scope.ScopeName, scope.ScopeID)
		if len(rules) == 0 {
			continue
		}
		re.logger.Debug("[RoutingEngine] Loaded %d rules for scope=%s, scopeID=%s", len(rules), scope.ScopeName, scope.ScopeID)
		rulesPerScope[scope] = rules
	}

	if len(rulesPerScope) == 0 {
		re.logger.Debug("[RoutingEngine] No routing rules found for any scope, skipping evaluation")
		return nil, nil
	}

	ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo,
		fmt.Sprintf("Evaluating routing rules for model=%s, provider=%s, requestType=%s", routingCtx.Model, routingCtx.Provider, routingCtx.RequestType))
	ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo, fmt.Sprintf("Scope chain: %v", scopeChainToStrings(scopeChain)))

	var finalDecision *RoutingDecision

	for chainStep := 0; ; chainStep++ {
		// TERMINATION 4: Chain exceeded configured max depth.
		maxDepth := *re.chainMaxDepth
		if chainStep >= maxDepth {
			re.logger.Warn("[RoutingEngine] Routing rule chain exceeded max depth (%d), stopping", maxDepth)
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelWarn, fmt.Sprintf("Chain exceeded max depth (%d) at step %d, stopping. Final resolved: provider=%s, model=%s", maxDepth, chainStep, currentProvider, currentModel))
			break
		}

		if chainStep > 0 {
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo, fmt.Sprintf("Chain step %d: re-evaluating with provider=%s, model=%s", chainStep, currentProvider, currentModel))
		}

		// Build CEL variables for the current chain step's provider/model.
		iterCtx := *routingCtx
		iterCtx.Provider = currentProvider
		iterCtx.Model = currentModel
		// Refresh budget/rate-limit status for the current provider/model so chained
		// rules that test budget_used, tokens_used, or request see fresh data.
		iterCtx.BudgetAndRateLimitStatus = re.store.GetBudgetAndRateLimitStatus(ctx, currentModel, currentProvider, routingCtx.VirtualKey, nil, nil, nil)

		variables, err := extractRoutingVariables(&iterCtx)
		if err != nil {
			re.logger.Error("[RoutingEngine] Failed to extract routing variables: %v", err)
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelError, fmt.Sprintf("Failed to extract routing variables: %v", err))
			return nil, fmt.Errorf("failed to extract routing variables: %w", err)
		}

		re.logger.Debug("[RoutingEngine] Chain Step: %d", chainStep)

		var stepDecision *RoutingDecision
		var matchedRule *configstoreTables.TableRoutingRule

	outerLoop:
		for _, scope := range scopeChain {
			rules, ok := rulesPerScope[scope]
			if !ok {
				continue
			}
			re.logger.Debug("[RoutingEngine] Evaluating scope=%s, scopeID=%s, ruleCount=%d", scope.ScopeName, scope.ScopeID, len(rules))

			ruleNames := make([]string, 0, len(rules))
			for _, r := range rules {
				ruleNames = append(ruleNames, r.Name)
			}

			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo, fmt.Sprintf("Evaluating scope %s: %d rules [%s]", scope.ScopeName, len(rules), strings.Join(ruleNames, ", ")))

			for _, rule := range rules {
				if _, fired := visitedRuleIDs[rule.ID]; fired {
					re.logger.Debug("[RoutingEngine] Skipping rule %s (already fired this chain)", rule.Name)
					ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo, fmt.Sprintf("Rule '%s' skipped: already fired in this chain", rule.Name))
					continue
				}
				re.logger.Debug("[RoutingEngine] Evaluating rule: name=%s, expression=%s", rule.Name, rule.CelExpression)

				program, err := re.store.GetRoutingProgram(ctx, rule)
				if err != nil {
					re.logger.Warn("[RoutingEngine] Failed to compile rule %s: %v", rule.Name, err)
					ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelError, fmt.Sprintf("Rule '%s' skipped: compile error: %v", rule.Name, err))
					continue
				}

				matched, err := evaluateCELExpression(program, variables)
				if err != nil {
					re.logger.Warn("[RoutingEngine] Failed to evaluate rule %s: %v", rule.Name, err)
					ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelError, fmt.Sprintf("Rule '%s' skipped: eval error: %v", rule.Name, err))
					continue
				}

				re.logger.Debug("[RoutingEngine] Rule %s evaluation result: matched=%v", rule.Name, matched)

				if !matched {
					ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo,
						fmt.Sprintf("Rule '%s' [%s] → no match (%s)", rule.Name, rule.CelExpression, buildNoMatchContext(rule.CelExpression, variables)))
					continue
				}

				provider := string(currentProvider)
				if rule.Provider != nil && *rule.Provider != "" {
					provider = *rule.Provider
				}

				model := currentModel
				if rule.Model != nil && *rule.Model != "" {
					model = *rule.Model
				}

				keyID := ""
				if rule.KeyID != nil {
					keyID = *rule.KeyID
				}

				stepDecision = &RoutingDecision{
					Provider:        provider,
					Model:           model,
					KeyID:           keyID,
					Fallbacks:       rule.ParsedFallbacks,
					MatchedRuleID:   rule.ID,
					MatchedRuleName: rule.Name,
				}
				matchedRule = rule
				break outerLoop
			}
		}

		// TERMINATION 1: No rule matched this iteration.
		if stepDecision == nil {
			break
		}

		// Accumulate: last match wins for all fields.
		finalDecision = stepDecision
		ctx.SetValue(schemas.BifrostContextKeyGovernanceRoutingRuleID, stepDecision.MatchedRuleID)
		ctx.SetValue(schemas.BifrostContextKeyGovernanceRoutingRuleName, stepDecision.MatchedRuleName)

		chainSuffix := ""
		if matchedRule.ChainRule {
			chainSuffix = " [chain_rule=true, continuing]"
		}
		re.logger.Debug("[RoutingEngine] Rule matched! Selected target: provider=%s, model=%s, fallbacks=%v%s", stepDecision.Provider, stepDecision.Model, stepDecision.Fallbacks, chainSuffix)
		ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo, fmt.Sprintf("Rule '%s' [%s] → matched, selected target: provider=%s, model=%s, fallbacks=%v%s", matchedRule.Name, matchedRule.CelExpression, stepDecision.Provider, stepDecision.Model, stepDecision.Fallbacks, chainSuffix))

		// TERMINATION 2: Rule is terminal (chain_rule=false, the default).
		if !matchedRule.ChainRule {
			break
		}

		// Mark this chain-rule as fired; it will be skipped in all subsequent chain steps.
		visitedRuleIDs[matchedRule.ID] = struct{}{}

		// Advance context for next chain iteration.
		currentProvider = schemas.ModelProvider(stepDecision.Provider)
		currentModel = stepDecision.Model
	}

	if finalDecision == nil {
		re.logger.Debug("[RoutingEngine] No routing rule matched, using default routing")
		ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo,
			fmt.Sprintf("SUMMARY: No routing rule matched; using %s/%s", routingCtx.Provider, routingCtx.Model))
	} else {
		requested := fmt.Sprintf("%s/%s", routingCtx.Provider, routingCtx.Model)
		resolved := fmt.Sprintf("%s/%s", finalDecision.Provider, finalDecision.Model)
		if routingCtx.Model != finalDecision.Model || string(routingCtx.Provider) != finalDecision.Provider {
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo,
				fmt.Sprintf("SUMMARY: Requested %s → routed to %s via rule '%s'", requested, resolved, finalDecision.MatchedRuleName))
		} else {
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineRoutingRule, schemas.LogLevelInfo,
				fmt.Sprintf("SUMMARY: Rule '%s' matched; kept %s", finalDecision.MatchedRuleName, resolved))
		}
	}
	return finalDecision, nil
}

// buildScopeChain builds the scope evaluation chain based on organizational hierarchy
// Returns scope levels in precedence order (highest to lowest)
// VirtualKey > Org (each ancestor) > Global
func buildScopeChain(virtualKey *configstoreTables.TableVirtualKey, orgAncestors []string) []ScopeLevel {
	var chain []ScopeLevel

	if virtualKey != nil {
		chain = append(chain, ScopeLevel{
			ScopeName: "virtual_key",
			ScopeID:   virtualKey.ID,
		})

		for _, orgID := range orgAncestors {
			chain = append(chain, ScopeLevel{
				ScopeName: "org",
				ScopeID:   orgID,
			})
		}
	}

	chain = append(chain, ScopeLevel{
		ScopeName: "global",
		ScopeID:   "",
	})

	return chain
}

// evaluateCELExpression evaluates a compiled CEL program with given variables
func evaluateCELExpression(program cel.Program, variables map[string]any) (bool, error) {
	if program == nil {
		return false, fmt.Errorf("CEL program is nil")
	}

	// Evaluate the program
	out, _, err := program.Eval(variables)
	if err != nil {
		// Gracefully handle "no such key" errors - when a header/param is missing, treat as non-match
		if strings.Contains(err.Error(), "no such key") {
			return false, nil
		}
		return false, fmt.Errorf("CEL evaluation error: %w", err)
	}

	// Convert result to boolean
	matched, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression did not return boolean, got: %T", out.Value())
	}

	return matched, nil
}

// extractRoutingVariables builds a map of CEL variables from routing context
// This map is used to evaluate CEL expressions in routing rules
func extractRoutingVariables(ctx *RoutingContext) (map[string]interface{}, error) {
	if ctx == nil {
		return nil, fmt.Errorf("routing context cannot be nil")
	}

	variables := make(map[string]interface{})

	// Basic request context
	variables["model"] = ctx.Model
	variables["provider"] = string(ctx.Provider)
	variables["request_type"] = ctx.RequestType // Normalized request type (e.g., "chat_completion", "embedding")

	// Headers and params - normalize headers to lowercase keys for case-insensitive CEL matching
	// This allows CEL expressions like headers["content-type"] to work regardless of how the header was sent
	normalizedHeaders := make(map[string]string)
	if ctx.Headers != nil {
		for k, v := range ctx.Headers {
			// Store with lowercase key for case-insensitive matching in CEL
			normalizedHeaders[strings.ToLower(k)] = v
		}
	}
	variables["headers"] = normalizedHeaders

	// Normalize query params to lowercase keys for case-insensitive CEL matching
	normalizedParams := make(map[string]string)
	if ctx.QueryParams != nil {
		for k, v := range ctx.QueryParams {
			normalizedParams[strings.ToLower(k)] = v
		}
	}
	variables["params"] = normalizedParams

	// Extract VirtualKey context if available
	if ctx.VirtualKey != nil {
		variables["virtual_key_id"] = ctx.VirtualKey.ID
		variables["virtual_key_name"] = ctx.VirtualKey.Name
	} else {
		variables["virtual_key_id"] = ""
		variables["virtual_key_name"] = ""
	}

	if ctx.VirtualKey != nil {
		variables["org_id"] = ctx.VirtualKey.GovernanceScopeOrgIDString()
	} else {
		variables["org_id"] = ""
	}
	// Deprecated CEL variables kept for backward compatibility with existing rules.
	variables["team_id"] = ""
	variables["team_name"] = ""
	variables["customer_id"] = ""
	variables["customer_name"] = ""

	// Populate budget and rate limit variables for current provider/model combination
	if ctx.BudgetAndRateLimitStatus != nil {
		variables["budget_used"] = ctx.BudgetAndRateLimitStatus.BudgetPercentUsed
		variables["tokens_used"] = ctx.BudgetAndRateLimitStatus.RateLimitTokenPercentUsed
		variables["request"] = ctx.BudgetAndRateLimitStatus.RateLimitRequestPercentUsed
		variables["soft_limit_exceeded"] = ctx.BudgetAndRateLimitStatus.SoftLimitExceeded
	} else {
		// No budget/rate limit configured, provide 0 values
		variables["budget_used"] = 0.0
		variables["tokens_used"] = 0.0
		variables["request"] = 0.0
		variables["soft_limit_exceeded"] = false
	}

	return variables, nil
}

// scopeChainToStrings converts a scope chain to a string representation for logging
func scopeChainToStrings(chain []ScopeLevel) []string {
	scopes := make([]string, 0, len(chain))
	for _, scope := range chain {
		if scope.ScopeID == "" {
			scopes = append(scopes, scope.ScopeName)
		} else {
			scopes = append(scopes, fmt.Sprintf("%s(%s)", scope.ScopeName, scope.ScopeID))
		}
	}
	return scopes
}

// buildNoMatchContext builds a compact debug string of scalar variables plus
// only the headers/params keys actually referenced in the CEL expression.
func buildNoMatchContext(expr string, variables map[string]any) string {
	parts := []string{
		fmt.Sprintf("model=%q", variables["model"]),
		fmt.Sprintf("provider=%q", variables["provider"]),
		fmt.Sprintf("request_type=%q", variables["request_type"]),
		fmt.Sprintf("budget_used=%.1f%%", variables["budget_used"]),
		fmt.Sprintf("tokens_used=%.1f%%", variables["tokens_used"]),
		fmt.Sprintf("request=%.1f%%", variables["request"]),
		fmt.Sprintf("soft_limit_exceeded=%v", variables["soft_limit_exceeded"]),
	}
	for _, mapName := range []string{"headers", "params"} {
		keys := extractMapKeysFromCEL(expr, mapName)
		if len(keys) == 0 {
			continue
		}
		if m, ok := variables[mapName].(map[string]string); ok {
			kvs := make([]string, 0, len(keys))
			for _, k := range keys {
				if _, exists := m[k]; exists {
					kvs = append(kvs, k+"=<present>")
				} else {
					kvs = append(kvs, k+"=<missing>")
				}
			}
			parts = append(parts, mapName+"("+strings.Join(kvs, ", ")+")")
		}
	}
	return strings.Join(parts, ", ")
}

// celMapKeyRegexCache caches one *regexp.Regexp per mapName to avoid
// recompiling on every call. Lazy and concurrent-safe via sync.Map's
// LoadOrStore atomicity; benign duplicate compiles on first concurrent miss.
var celMapKeyRegexCache sync.Map // map[string]*regexp.Regexp

// extractMapKeysFromCEL extracts unique map access keys for mapName from a CEL expression.
// Handles mapName["key"], mapName['key'], and mapName.key patterns.
func extractMapKeysFromCEL(expr, mapName string) []string {
	v, ok := celMapKeyRegexCache.Load(mapName)
	if !ok {
		quoted := regexp.QuoteMeta(mapName)
		compiled := regexp.MustCompile(quoted + `\["([^"]+)"\]|` + quoted + `\['([^']+)'\]|` + quoted + `\.([a-zA-Z_][a-zA-Z0-9_]*)`)
		v, _ = celMapKeyRegexCache.LoadOrStore(mapName, compiled)
	}
	re := v.(*regexp.Regexp)
	seen := map[string]struct{}{}
	var keys []string
	for _, m := range re.FindAllStringSubmatch(expr, -1) {
		for _, cap := range m[1:] {
			if cap != "" {
				if _, dup := seen[cap]; !dup {
					seen[cap] = struct{}{}
					keys = append(keys, cap)
				}
				break
			}
		}
	}
	return keys
}

// createCELEnvironment creates a new CEL environment for routing rules
func createCELEnvironment() (*cel.Env, error) {
	return cel.NewEnv(
		// Basic request context
		cel.Variable("model", cel.StringType),
		cel.Variable("provider", cel.StringType),
		cel.Variable("request_type", cel.StringType), // Normalized request type (e.g., "chat_completion", "embedding", "text_completion")

		// Headers and params (dynamic from request)
		cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("params", cel.MapType(cel.StringType, cel.StringType)),

		// VirtualKey/Team/Customer context
		cel.Variable("virtual_key_id", cel.StringType),
		cel.Variable("virtual_key_name", cel.StringType),
		cel.Variable("org_id", cel.StringType),
		cel.Variable("team_id", cel.StringType),
		cel.Variable("team_name", cel.StringType),
		cel.Variable("customer_id", cel.StringType),
		cel.Variable("customer_name", cel.StringType),

		// Rate limit & budget status (real-time capacity metrics as percentages)
		cel.Variable("tokens_used", cel.DoubleType),
		cel.Variable("request", cel.DoubleType),
		cel.Variable("budget_used", cel.DoubleType),

		// True when any soft-limit budget or rate limit has crossed its threshold.
		// Use this instead of budget_used/tokens_used/request >= 100 when you only
		// want to act on soft limits and leave hard-limit behaviour unchanged.
		cel.Variable("soft_limit_exceeded", cel.BoolType),
	)
}
