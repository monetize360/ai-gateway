package governance

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

const (
	// contextSafetyMargin is the headroom kept between the estimated input size and a
	// model's advertised limit, absorbing the error in charsPerTokenEstimate.
	contextSafetyMargin = 0.15

	// semanticRoutingBudget bounds the in-process work around model selection.
	// When vLLM-SR is configured, its explicit HTTP timeout is added separately.
	semanticRoutingBudget = 10 * time.Millisecond

	// unknownLimitTokenCeiling bounds requests routed to a model whose token limits the
	// catalog does not carry. Every current chat model comfortably exceeds this, so a
	// request under the ceiling is safe even without a known limit.
	unknownLimitTokenCeiling = 8000

	// assumedOutputTokens is used for cost comparison when the caller did not pin an
	// output limit. Only relative ordering matters, so the absolute value is arbitrary.
	assumedOutputTokens = 512
)

// Default scoring weights.
const (
	defaultPreferenceWeight = 0.5
	defaultTaskWeight       = 1.0
	defaultTierWeight       = 1.0
	defaultCostWeight       = 0.5
	defaultContextWeight    = 0.2
	defaultMaxFallbacks     = 2
)

// SemanticRoutingConfig configures capability-aware semantic routing. Routing is off
// unless Enabled is set. Enabled is a kill switch: when false, semantic selection
// declines and load balancing takes over.
type SemanticRoutingConfig struct {
	Enabled bool `json:"enabled"`

	// DefaultForAll, when true with Enabled, runs semantic for every VK-backed
	// request that did not match a pin rule.
	DefaultForAll bool `json:"default_for_all"`

	// Classes maps a class name to an ordered model preference list used when
	// ranking L1 survivors alongside a vLLM-SR recommendation (and for
	// preferenceOverride from routing rules). Not a standalone Layer 2 router.
	Classes map[string][]string `json:"classes,omitempty"`

	// DefaultClass names the preference key used when ranking without a class override.
	DefaultClass string `json:"default_class,omitempty"`

	TaskWeight       *float64 `json:"task_weight,omitempty"`
	TierWeight       *float64 `json:"tier_weight,omitempty"`
	PreferenceWeight *float64 `json:"preference_weight,omitempty"`
	CostWeight       *float64 `json:"cost_weight,omitempty"`
	ContextWeight    *float64 `json:"context_weight,omitempty"`

	MaxFallbacks *int `json:"max_fallbacks,omitempty"`

	// Router configures vLLM-SR as the Layer 2 decision sidecar.
	Router *VLLMSRRouterConfig `json:"router,omitempty"`

	// Classifier is deprecated and ignored. Kept for config compatibility only.
	Classifier *DeprecatedClassifierConfig `json:"classifier,omitempty"`

	// ComplexityLowMax / MidMax / ReasoningRequireMin / TaskTypeProbMin /
	// ClassificationTTL are deprecated and ignored (classifier/lexical path removed).
	ComplexityLowMax    *float64      `json:"complexity_low_max,omitempty"`
	ComplexityMidMax    *float64      `json:"complexity_mid_max,omitempty"`
	ReasoningRequireMin *float64      `json:"reasoning_require_min,omitempty"`
	TaskTypeProbMin     *float64      `json:"task_type_prob_min,omitempty"`
	ClassificationTTL   time.Duration `json:"classification_ttl,omitempty"`

	// ModelOverrides annotate provider/model pairs with routing metadata.
	// Prefer inline overrides (or catalog DB fields) in production.
	ModelOverrides map[string]modelcatalog.ModelRoutingOverride `json:"model_overrides,omitempty"`

	// ModelOverridesFile is an optional path to a JSON catalog of routing metadata.
	// Relative paths resolve against the process working directory / -app-dir.
	// Entries are loaded at plugin init and merged with ModelOverrides (inline wins).
	// Not required for in-process semantic-router plugin mode (recipe_file is enough).
	ModelOverridesFile string `json:"model_overrides_file,omitempty"`

	AllowPreviewModels bool `json:"allow_preview_models,omitempty"`
}

func (c *SemanticRoutingConfig) preferenceWeight() float64 {
	if c.PreferenceWeight != nil {
		return *c.PreferenceWeight
	}
	return defaultPreferenceWeight
}

func (c *SemanticRoutingConfig) taskWeight() float64 {
	if c.TaskWeight != nil {
		return *c.TaskWeight
	}
	return defaultTaskWeight
}

func (c *SemanticRoutingConfig) tierWeight() float64 {
	if c.TierWeight != nil {
		return *c.TierWeight
	}
	return defaultTierWeight
}

func (c *SemanticRoutingConfig) costWeight() float64 {
	if c.CostWeight != nil {
		return *c.CostWeight
	}
	return defaultCostWeight
}

func (c *SemanticRoutingConfig) contextWeight() float64 {
	if c.ContextWeight != nil {
		return *c.ContextWeight
	}
	return defaultContextWeight
}

func (c *SemanticRoutingConfig) maxFallbacks() int {
	if c.MaxFallbacks != nil {
		return *c.MaxFallbacks
	}
	return defaultMaxFallbacks
}

func (p *GovernancePlugin) semanticRoutingConfig() *SemanticRoutingConfig {
	p.cfgMutex.RLock()
	defer p.cfgMutex.RUnlock()
	return p.semanticRouting
}

// routeCandidate is one (provider, model) pair the request could be sent to.
type routeCandidate struct {
	Provider schemas.ModelProvider
	Model    string
	Config   configstoreTables.TableAllowedModelConfig

	preferenceIndex int
	taskScore       float64
	tierScore       float64
	estCost         float64
	hasCost         bool
	contextScore    float64
	costScore       float64
	score           float64

	card          *modelCard
	capabilityFit float64
}

func (c routeCandidate) qualified() string {
	return string(c.Provider) + "/" + c.Model
}

func (p *GovernancePlugin) enumerateCandidates(ctx *schemas.BifrostContext, comp *tenantGovernanceComponents, virtualKey *configstoreTables.TableVirtualKey) []routeCandidate {
	if virtualKey == nil || len(virtualKey.AllowedModelConfigs) == 0 {
		return nil
	}

	blockedProviders := make(map[string]bool)
	for _, config := range virtualKey.AllowedModelConfigs {
		if config.BlacklistedModels.IsBlockAll() {
			blockedProviders[config.Provider] = true
		}
	}

	candidates := make([]routeCandidate, 0, len(virtualKey.AllowedModelConfigs))
	accessSkipped := 0
	for _, config := range virtualKey.AllowedModelConfigs {
		if blockedProviders[config.Provider] {
			continue
		}
		if comp.resolver != nil {
			if comp.resolver.isProviderBudgetViolated(ctx, virtualKey, config) || comp.resolver.isProviderRateLimitViolated(ctx, virtualKey, config) {
				continue
			}
		}

		provider := schemas.ModelProvider(config.Provider)
		for _, model := range p.modelsForConfig(provider, config) {
			if isModelBlockedByList(config.BlacklistedModels, model) {
				continue
			}
			if comp.resolver != nil && !comp.resolver.isProviderAndModelAccessible(virtualKey, provider, model) {
				accessSkipped++
				continue
			}
			candidates = append(candidates, routeCandidate{Provider: provider, Model: model, Config: config})
		}
	}

	if accessSkipped > 0 {
		p.logSemantic(ctx, schemas.LogLevelDebug, "1/pool: skipped %d models by org/VK access", accessSkipped)
	}
	return candidates
}

func (p *GovernancePlugin) modelsForConfig(provider schemas.ModelProvider, config configstoreTables.TableAllowedModelConfig) []string {
	if config.AllowedModels.IsEmpty() {
		return nil
	}

	if config.AllowedModels.IsUnrestricted() {
		if p.modelCatalog == nil {
			return nil
		}
		return p.modelCatalog.GetModelsForProvider(provider)
	}

	models := make([]string, 0, len(config.AllowedModels))
	for _, entry := range config.AllowedModels {
		entryProvider, model := schemas.ParseModelString(entry, provider)
		if entryProvider != "" && entryProvider != provider {
			continue
		}
		if model != "" {
			models = append(models, model)
		}
	}
	return models
}

// candidateSatisfiesProfile is the hard post-router capability filter.
// Layer 2 sees the VK pool before this catalog/capability validation runs.
func (p *GovernancePlugin) candidateSatisfiesProfile(candidate routeCandidate, profile *RequestProfile, requestType schemas.RequestType) (bool, string) {
	if p.modelCatalog == nil {
		return false, "model catalog unavailable"
	}

	entry := p.routingEntry(candidate)
	if entry == nil {
		if candidate.card != nil {
			return cardSatisfiesRequest(candidate.card, profile)
		}
		return false, "no catalog entry"
	}

	if ok, reason := statusAllowed(string(entry.Status), p.semanticRoutingConfig()); !ok {
		return false, reason
	}

	if requestType != "" && !p.modelCatalog.IsRequestTypeSupported(candidate.Model, candidate.Provider, requestType) {
		return false, fmt.Sprintf("request type %s unsupported", requestType)
	}

	params := p.modelCatalog.GetSupportedParameters(candidate.Model)
	if profile.HasImage && !hasCapability(params, modelcatalog.CapabilityVision, entry.SupportsVision) {
		return false, "no image input support"
	}
	if profile.HasPDF && !hasCapability(params, modelcatalog.CapabilityPDFInput, entry.SupportsPDFInput) {
		return false, "no file input support"
	}
	if profile.HasAudio && !hasCapability(params, modelcatalog.CapabilityAudioInput, entry.SupportsAudioInput) {
		return false, "no audio input support"
	}
	if profile.HasTools && !hasCapability(params, "tools", nil) {
		return false, "no tool calling support"
	}
	if profile.NeedsJSONSchema && !hasCapability(params, "response_format", nil) {
		return false, "no structured output support"
	}

	if ok, reason := fitsContext(entry, profile); !ok {
		return false, reason
	}

	return true, ""
}

func statusAllowed(status string, cfg *SemanticRoutingConfig) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", string(modelcatalog.ModelStatusLive):
		return true, ""
	case string(modelcatalog.ModelStatusPreview):
		if cfg != nil && cfg.AllowPreviewModels {
			return true, ""
		}
		return false, "preview model not allowed"
	case string(modelcatalog.ModelStatusDeprecated):
		return false, "deprecated model"
	default:
		return false, "unknown model status"
	}
}

func hasCapability(params []string, param string, flag *bool) bool {
	if flag != nil && *flag {
		return true
	}
	for _, p := range params {
		if p == param {
			return true
		}
	}
	return false
}

func fitsContext(entry *modelcatalog.PricingEntry, profile *RequestProfile) (bool, string) {
	maxInput := entry.MaxInputTokens
	if maxInput == nil {
		maxInput = entry.ContextLength
	}

	switch {
	case maxInput != nil:
		needed := int(float64(profile.EstInputTokens) * (1 + contextSafetyMargin))
		if needed > *maxInput {
			return false, fmt.Sprintf("input ~%d tokens exceeds limit %d", profile.EstInputTokens, *maxInput)
		}
	case profile.EstInputTokens > unknownLimitTokenCeiling:
		return false, fmt.Sprintf("input ~%d tokens with unknown limit", profile.EstInputTokens)
	}

	if profile.RequestedMaxTokens > 0 && entry.MaxOutputTokens != nil && profile.RequestedMaxTokens > *entry.MaxOutputTokens {
		return false, fmt.Sprintf("requested %d output tokens exceeds limit %d", profile.RequestedMaxTokens, *entry.MaxOutputTokens)
	}

	return true, ""
}

func (p *GovernancePlugin) routingEntry(candidate routeCandidate) *modelcatalog.PricingEntry {
	if p.modelCatalog == nil {
		return nil
	}
	entry := p.modelCatalog.GetModelCapabilityEntryForModel(candidate.Model, candidate.Provider)
	cfg := p.semanticRoutingConfig()
	if cfg == nil || len(cfg.ModelOverrides) == 0 {
		return entry
	}
	override, ok := lookupModelOverride(cfg.ModelOverrides, candidate)
	if !ok {
		return entry
	}
	return modelcatalog.ApplyRoutingOverride(entry, override)
}

func lookupModelOverride(overrides map[string]modelcatalog.ModelRoutingOverride, candidate routeCandidate) (modelcatalog.ModelRoutingOverride, bool) {
	if override, ok := overrides[candidate.qualified()]; ok {
		return override, true
	}
	if override, ok := overrides[candidate.Model]; ok {
		return override, true
	}
	return modelcatalog.ModelRoutingOverride{}, false
}

// rankCandidates scores L1 survivors by preference list, cost, context, and
// mid-tier capability (no prompt classification — Layer 2 is vLLM-SR).
func (p *GovernancePlugin) rankCandidates(candidates []routeCandidate, profile *RequestProfile, cfg *SemanticRoutingConfig, preferenceOverride []string) []routeCandidate {
	preferences := cfg.Classes[cfg.DefaultClass]
	if len(preferenceOverride) > 0 {
		preferences = preferenceOverride
	}

	minCost, maxCost := 0.0, 0.0
	haveCostRange := false
	for i := range candidates {
		candidates[i].preferenceIndex = preferenceIndex(preferences, candidates[i])
		candidates[i].estCost, candidates[i].hasCost = p.estimateCost(candidates[i], profile)
		candidates[i].contextScore = contextScore(p.contextLimit(candidates[i]), profile)
		candidates[i].taskScore = 0.5 // neutral without a classification
		candidates[i].tierScore = tierMatchScore(p.routingEntry(candidates[i]))

		if candidates[i].hasCost {
			if !haveCostRange {
				minCost, maxCost = candidates[i].estCost, candidates[i].estCost
				haveCostRange = true
				continue
			}
			minCost = min(minCost, candidates[i].estCost)
			maxCost = max(maxCost, candidates[i].estCost)
		}
	}

	for i := range candidates {
		costScore := 0.5
		if candidates[i].hasCost && haveCostRange {
			if maxCost > minCost {
				costScore = 1 - (candidates[i].estCost-minCost)/(maxCost-minCost)
			} else {
				costScore = 1
			}
		}
		candidates[i].costScore = costScore

		preferenceScore := 0.0
		if idx := candidates[i].preferenceIndex; idx >= 0 && len(preferences) > 0 {
			preferenceScore = 1 - float64(idx)/float64(len(preferences))
		}

		candidates[i].score = cfg.taskWeight()*candidates[i].taskScore +
			cfg.tierWeight()*candidates[i].tierScore +
			cfg.preferenceWeight()*preferenceScore +
			cfg.costWeight()*costScore +
			cfg.contextWeight()*candidates[i].contextScore
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.taskScore != b.taskScore {
			return a.taskScore > b.taskScore
		}
		if a.hasCost && b.hasCost && a.estCost != b.estCost {
			return a.estCost < b.estCost
		}
		if a.contextScore != b.contextScore {
			return a.contextScore > b.contextScore
		}
		if a.preferenceIndex != b.preferenceIndex {
			if a.preferenceIndex < 0 || b.preferenceIndex < 0 {
				return b.preferenceIndex < 0
			}
			return a.preferenceIndex < b.preferenceIndex
		}
		return a.qualified() < b.qualified()
	})

	return candidates
}

func tierMatchScore(entry *modelcatalog.PricingEntry) float64 {
	tier := modelcatalog.CapabilityTierMid
	if entry != nil && entry.CapabilityTier != "" {
		tier = modelcatalog.NormalizeCapabilityTier(string(entry.CapabilityTier))
	}
	switch tier {
	case modelcatalog.CapabilityTierMid, modelcatalog.CapabilityTierLarge:
		return 1
	case modelcatalog.CapabilityTierFlagship:
		return 0.7
	case modelcatalog.CapabilityTierSmall:
		return 0.5
	case modelcatalog.CapabilityTierNano:
		return 0.2
	}
	return 0.5
}

func preferenceIndex(preferences []string, candidate routeCandidate) int {
	for i, entry := range preferences {
		entryProvider, entryModel := schemas.ParseModelString(entry, "")
		if entryProvider != "" && entryProvider != candidate.Provider {
			continue
		}
		if strings.EqualFold(entryModel, candidate.Model) {
			return i
		}
	}
	return -1
}

func (p *GovernancePlugin) estimateCost(candidate routeCandidate, profile *RequestProfile) (float64, bool) {
	if p.modelCatalog == nil {
		return 0, false
	}
	inputRate, outputRate := p.modelCatalog.GetChatTokenRates(string(candidate.Provider), candidate.Model)
	if inputRate == nil && outputRate == nil {
		return 0, false
	}

	outputTokens := profile.RequestedMaxTokens
	if outputTokens <= 0 {
		outputTokens = assumedOutputTokens
	}

	cost := 0.0
	if inputRate != nil {
		cost += float64(profile.EstInputTokens) * *inputRate
	}
	if outputRate != nil {
		cost += float64(outputTokens) * *outputRate
	}
	return cost, true
}

func (p *GovernancePlugin) contextLimit(candidate routeCandidate) int {
	entry := p.routingEntry(candidate)
	if entry == nil {
		return 0
	}
	if entry.MaxInputTokens != nil {
		return *entry.MaxInputTokens
	}
	if entry.ContextLength != nil {
		return *entry.ContextLength
	}
	return 0
}

func contextScore(limit int, profile *RequestProfile) float64 {
	if limit <= 0 || profile.EstInputTokens <= 0 {
		return 0.5
	}
	utilization := float64(profile.EstInputTokens) / float64(limit)
	if utilization >= 1 {
		return 0
	}
	return 1 - utilization
}

func (p *GovernancePlugin) validateCandidate(comp *tenantGovernanceComponents, virtualKey *configstoreTables.TableVirtualKey, candidate routeCandidate) (string, bool) {
	if p.modelCatalog == nil || p.inMemoryStore == nil {
		return "", false
	}

	if comp != nil && comp.resolver != nil && virtualKey != nil {
		if !comp.resolver.isProviderAndModelAccessible(virtualKey, candidate.Provider, candidate.Model) {
			return "", false
		}
	}

	providerConfig, configured := p.inMemoryStore.GetConfiguredProviders()[candidate.Provider]
	if !configured {
		return "", false
	}
	if !p.modelCatalog.IsModelAllowedForProvider(candidate.Provider, candidate.Model, &providerConfig, candidate.Config.AllowedModels) {
		return "", false
	}

	refined, err := p.modelCatalog.RefineModelForProvider(candidate.Provider, candidate.Model)
	if err != nil {
		p.logger.Debug("[Governance] semantic routing: cannot refine %s: %v", candidate.qualified(), err)
		return "", false
	}
	return refined, true
}

// logSemantic writes one semantic-routing line to the request's routing-engine log
// and the process logger.
func (p *GovernancePlugin) logSemantic(ctx *schemas.BifrostContext, level schemas.LogLevel, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ctx != nil {
		ctx.AppendRoutingEngineLog(schemas.RoutingEngineSemantic, level, msg)
	}
	if p == nil || p.logger == nil {
		return
	}
	line := "[Governance] semantic routing: " + msg
	switch level {
	case schemas.LogLevelError:
		p.logger.Error("%s", line)
	case schemas.LogLevelWarn:
		p.logger.Warn("%s", line)
	default:
		p.logger.Info("%s", line)
	}
}

// shouldApplySemanticRouting is the Layer 1 handoff gate. It does not re-evaluate
// CEL; it only reads the RoutingDecision already produced by EvaluateRoutingRules.
func (p *GovernancePlugin) shouldApplySemanticRouting(ctx *schemas.BifrostContext, virtualKey *configstoreTables.TableVirtualKey, decision *RoutingDecision, hasIncomingModel bool) (run bool, preferenceOverride []string, handoff string) {
	if virtualKey == nil {
		if decision != nil && decision.IsSemanticRouting() {
			p.logSemantic(ctx, schemas.LogLevelWarn, "Rule %q matched semantic_routing but no virtual key is present; declining; keeping incoming model", decision.MatchedRuleName)
		}
		return false, nil, ""
	}
	cfg := p.semanticRoutingConfig()
	if cfg == nil || !cfg.Enabled {
		return false, nil, ""
	}
	if decision != nil {
		if !decision.IsSemanticRouting() {
			return false, nil, ""
		}
		return true, decision.Fallbacks, fmt.Sprintf("moving to semantic routing (rule=%q)", decision.MatchedRuleName)
	}
	// No CEL match: keep the requested model unless the operator opted into
	// default_for_all, or the request omitted model entirely.
	if cfg.DefaultForAll {
		return true, nil, "default semantic routing"
	}
	if !hasIncomingModel {
		return true, nil, "missing model; semantic routing"
	}
	return false, nil, ""
}

func (p *GovernancePlugin) applySemanticRouting(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, body map[string]any, virtualKey *configstoreTables.TableVirtualKey, preferenceOverride []string) (map[string]any, bool) {
	cfg := p.semanticRoutingConfig()
	routingBudget := semanticRoutingBudget
	// Layer 2 inference time is additive to the existing local governance-work
	// budget for both the HTTP and embedded runtimes.
	if cfg != nil && (cfg.usesHTTPRouter() || cfg.usesPluginRouter()) {
		routingBudget += cfg.Router.timeout()
	}
	routingDeadline := time.Now().Add(routingBudget)
	vkName := ""
	if virtualKey != nil {
		vkName = virtualKey.Name
	}

	p.logSemantic(ctx, schemas.LogLevelInfo, "Selecting model from virtual key %q pool", vkName)

	if cfg == nil || !cfg.Enabled {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: kill switch off (semantic_routing.enabled=false); keeping incoming model")
		return body, false
	}
	if semanticRoutingOptedOut(req) {
		p.logSemantic(ctx, schemas.LogLevelInfo, "Skipped: disabled by request header")
		return body, false
	}

	comp := p.getComponentsForContext(ctx)
	if comp == nil {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: governance store not available for this tenant")
		return body, false
	}

	resolved, hasIncomingModel := resolveRoutedModel(ctx, req, body)
	declineTail := "keeping incoming model"
	if !hasIncomingModel {
		declineTail = "no model on request and semantic did not select"
	}

	totalStart := time.Now()
	logSemanticTotal := func(routed bool) {
		p.logSemantic(ctx, schemas.LogLevelInfo, "SUMMARY: semantic routing finished routed=%t total_took=%s", routed, formatTook(time.Since(totalStart)))
	}

	p.logSemantic(ctx, schemas.LogLevelInfo, "1/extract: request profile starting")
	step := time.Now()
	profile := buildRequestProfile(body)
	p.logSemantic(ctx, schemas.LogLevelInfo, "1/extract: requestProfile %s took=%s", profile.logFields(), formatTook(time.Since(step)))
	if !profile.Recognized {
		p.logSemantic(ctx, schemas.LogLevelInfo, "Skipped: request body shape not recognized, cannot derive capability requirements; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}

	requestType := requestTypeFromContext(ctx)
	requested := resolved.Model
	if requested == "" {
		requested = "(none)"
	}
	p.logSemantic(ctx, schemas.LogLevelInfo, "1/extract: request_type=%s requested=%s", requestType, requested)
	if !slices.Contains(semanticRoutableRequestTypes, requestType) {
		p.logSemantic(ctx, schemas.LogLevelInfo, "Skipped: request type %s is not semantically routed; %s", requestType, declineTail)
		logSemanticTotal(false)
		return body, false
	}

	step = time.Now()
	candidates := p.enumerateCandidates(ctx, comp, virtualKey)
	p.logSemantic(ctx, schemas.LogLevelInfo, "1/pool: %d models on VK %q took=%s", len(candidates), vkName, formatTook(time.Since(step)))
	if len(candidates) == 0 {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: no candidate models on virtual key %q; %s", vkName, declineTail)
		logSemanticTotal(false)
		return body, false
	}

	if !time.Now().Before(routingDeadline) {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: semantic routing budget exhausted after VK pool lookup; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}

	step = time.Now()
	ranked, skipRewrite := p.selectWithLayer2(ctx, comp, body, profile, candidates, cfg, preferenceOverride)
	p.logSemantic(ctx, schemas.LogLevelInfo, "2/select: router-ordered %s took=%s", describeRanking(ranked), formatTook(time.Since(step)))
	if skipRewrite {
		p.logSemantic(ctx, schemas.LogLevelInfo, "Skipped: Layer 2 did not require a model rewrite; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}
	if !time.Now().Before(routingDeadline) {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: semantic routing budget exhausted after Layer 2 selection; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}
	if len(ranked) == 0 {
		logSemanticTotal(false)
		return body, false
	}

	step = time.Now()
	eligible := make([]routeCandidate, 0, len(ranked))
	rejections := make(map[string][]string, 4)
	for _, candidate := range ranked {
		if satisfied, reason := p.candidateSatisfiesProfile(candidate, profile, requestType); satisfied {
			eligible = append(eligible, candidate)
		} else {
			rejections[reason] = append(rejections[reason], candidate.qualified())
		}
	}
	p.logSemantic(ctx, schemas.LogLevelInfo, "3/filter: %d/%d router recommendations capable%s took=%s",
		len(eligible), len(ranked), describeRejections(rejections), formatTook(time.Since(step)))
	if len(eligible) == 0 {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: no Layer 2 recommendation satisfied the request profile; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}
	if !time.Now().Before(routingDeadline) {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: semantic routing budget exhausted after capability filter; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}

	step = time.Now()
	var (
		winner       *routeCandidate
		winnerModel  string
		runnersUp    []string
		maxFallbacks = cfg.maxFallbacks()
	)
	for i := range eligible {
		if !time.Now().Before(routingDeadline) {
			p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: semantic routing budget exhausted during final validation; %s", declineTail)
			logSemanticTotal(false)
			return body, false
		}
		refined, valid := p.validateCandidate(comp, virtualKey, eligible[i])
		if !valid {
			continue
		}
		if winner == nil {
			winner = &eligible[i]
			winnerModel = refined
			continue
		}
		if len(runnersUp) < maxFallbacks {
			runnersUp = append(runnersUp, string(eligible[i].Provider)+"/"+refined)
		}
	}

	if winner == nil {
		p.logSemantic(ctx, schemas.LogLevelWarn, "Skipped: no capable candidate passed final validation; %s", declineTail)
		logSemanticTotal(false)
		return body, false
	}

	writeModelBack(ctx, body, resolved.IsGeminiPath, resolved.IsBedrockPath, winner.Provider, winnerModel, resolved.GenAISuffix)
	schemas.AppendToContextList(ctx, schemas.BifrostContextKeyRoutingEnginesUsed, schemas.RoutingEngineSemantic)

	fallbacks := runnersUp
	if originalProvider, originalModel := schemas.ParseModelString(resolved.Model, ""); originalProvider != "" &&
		!(originalProvider == winner.Provider && strings.EqualFold(originalModel, winnerModel)) &&
		(comp.resolver == nil || comp.resolver.isProviderAndModelAccessible(virtualKey, originalProvider, originalModel)) {
		fallbacks = append([]string{string(originalProvider) + "/" + originalModel}, runnersUp...)
	}
	if setFallbacksIfAbsent(body, fallbacks) {
		p.logSemantic(ctx, schemas.LogLevelInfo, "Fallbacks: %v", fallbacks)
	}

	p.logSemantic(ctx, schemas.LogLevelInfo, "4/commit: selected provider=%s model=%s score=%.3f took=%s",
		winner.Provider, winnerModel, winner.score, formatTook(time.Since(step)))
	logSemanticTotal(true)

	return body, true
}

// selectWithLayer2 is Step 2: Layer 2 (in-process plugin or vLLM-SR HTTP)
// orders the models associated with the virtual key. Capability/catalog filtering
// intentionally happens afterward so Layer 1 never prevents the router call.
func (p *GovernancePlugin) selectWithLayer2(ctx *schemas.BifrostContext, comp *tenantGovernanceComponents, body map[string]any, profile *RequestProfile, pool []routeCandidate, cfg *SemanticRoutingConfig, preferenceOverride []string) ([]routeCandidate, bool) {
	if !p.layer2Enabled(cfg) {
		p.logSemantic(ctx, schemas.LogLevelInfo, "2/route: layer2 router unset; declining semantic rewrite")
		return nil, false
	}

	route, err := p.previewLayer2Route(ctx, body, profile, pool, cfg)
	if err != nil {
		p.logSemantic(ctx, schemas.LogLevelWarn, "2/route: layer2 failed (%v); declining semantic rewrite", err)
		return nil, false
	}
	if route.skipRewrite() {
		if route.Profile == nil {
			p.logSemantic(ctx, schemas.LogLevelInfo, "2/route: layer2 selection_status=%q selection_method=%q; no model rewrite required",
				route.SelectionStatus, route.SelectionMethod)
			return nil, true
		}
		ranked := p.rankByModelCards(ctx, comp, pool, profile, cfg, preferenceOverride, route)
		if len(ranked) == 0 {
			p.logSemantic(ctx, schemas.LogLevelInfo, "2/route: decision=%q no VK model card matched the capability profile; falling back to requested model",
				route.Decision)
			return nil, true
		}
		return ranked, false
	}

	// Score the VK pool for observability and deterministic tail fallbacks;
	// the Layer 2 selected model still controls the first position.
	locallyRanked := p.rankCandidates(pool, profile, cfg, preferenceOverride)
	ranked := intersectRouteWithEligible(locallyRanked, route)
	if len(ranked) == 0 {
		p.logSemantic(ctx, schemas.LogLevelWarn,
			"2/route: layer2 selected=%q and recommendations do not overlap the VK pool; declining semantic rewrite",
			route.SelectedModel)
		return nil, false
	}
	if winner := ranked[0].qualified(); !strings.EqualFold(winner, route.SelectedModel) {
		p.logSemantic(ctx, schemas.LogLevelInfo,
			"2/route: layer2 selected=%s decision=%q algorithm=%q status=%q; selected model not in VK pool, using next recommendation %s; ordering %d VK models",
			route.SelectedModel, route.Decision, route.Algorithm, route.SelectionStatus, winner, len(ranked))
		return ranked, false
	}
	p.logSemantic(ctx, schemas.LogLevelInfo,
		"2/route: layer2 selected=%s decision=%q algorithm=%q status=%q; ordering %d VK models",
		route.SelectedModel, route.Decision, route.Algorithm, route.SelectionStatus, len(ranked))
	return ranked, false
}

func formatTook(d time.Duration) string {
	return d.Round(time.Microsecond).String()
}

func describeRanking(ranked []routeCandidate) string {
	if len(ranked) == 0 {
		return "(empty)"
	}
	limit := min(5, len(ranked))
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		parts = append(parts, fmt.Sprintf("%s=%.3f", ranked[i].qualified(), ranked[i].score))
	}
	out := strings.Join(parts, ", ")
	if len(ranked) > limit {
		out += ", ..."
	}
	return out
}

// describeRejections names the dropped router recommendations, not just a count.
// This makes post-router capability and catalog decisions observable.
func describeRejections(rejections map[string][]string) string {
	if len(rejections) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(rejections))
	for reason, models := range rejections {
		sort.Strings(models)
		reasons = append(reasons, fmt.Sprintf("%s (%s)", reason, strings.Join(models, ", ")))
	}
	sort.Strings(reasons)
	return " — excluded: " + strings.Join(reasons, ", ")
}

func semanticRoutingOptedOut(req *schemas.HTTPRequest) bool {
	for name := range req.Headers {
		if strings.EqualFold(name, semanticRoutingDisableHeader) {
			return true
		}
	}
	return false
}

var semanticRoutableRequestTypes = []schemas.RequestType{
	schemas.ChatCompletionRequest,
	schemas.ResponsesRequest,
	schemas.TextCompletionRequest,
}

func requestTypeFromContext(ctx *schemas.BifrostContext) schemas.RequestType {
	val := ctx.Value(schemas.BifrostContextKeyHTTPRequestType)
	var requestType schemas.RequestType
	switch typed := val.(type) {
	case schemas.RequestType:
		requestType = typed
	case string:
		requestType = schemas.RequestType(typed)
	default:
		return ""
	}
	return schemas.RequestType(strings.TrimSuffix(string(requestType), "_stream"))
}

func (profile *RequestProfile) logFields() string {
	if profile == nil {
		return "recognized=false"
	}
	return fmt.Sprintf("recognized=%t input_chars=%d est_input_tokens=%d messages=%d image=%t pdf=%t audio=%t tools=%t tool_count=%d json_schema=%t streaming=%t requested_max_tokens=%d",
		profile.Recognized,
		profile.InputChars,
		profile.EstInputTokens,
		profile.MessageCount,
		profile.HasImage,
		profile.HasPDF,
		profile.HasAudio,
		profile.HasTools,
		profile.ToolCount,
		profile.NeedsJSONSchema,
		profile.IsStreaming,
		profile.RequestedMaxTokens,
	)
}
