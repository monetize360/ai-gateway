package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticrouter"
	"gorm.io/gorm"
)

const (
	catalogListingTable = "catalog_listings"
	picklistItemTable   = "picklist_item"

	// longContextTokenFloor is the context_length that counts as long_context
	// even when the category tag does not say so.
	longContextTokenFloor = 32768
)

// modelCard is the routing view of one published catalog listing.
type modelCard struct {
	Name                string
	Provider            string
	ExternalID          string
	InputModalities     []string
	OutputModalities    []string
	ContextWindow       int
	Capabilities        []string
	QualityIndex        float64 // 0..10; 0 when unknown
	TTFTP50Ms           float64 // 0 when unknown
	ThroughputTokensSec float64 // 0 when unknown; breaks score ties
	benchmarked         bool
}

type catalogRouteRow struct {
	ProviderName   *string
	ModelName      *string
	GatewayModelID *string
	ContextLength  *float64
	QualityIndex   *float64
	LatencyS       *float64
	Category       *string
	ListingStatus  *string
	InfraStatus    *string
	TTFTMs         *float64
	Throughput     *float64
}

// loadModelCards replaces the tenant's routing index from published catalog
// listings joined to infra capabilities and the latest benchmark.
// Tenants without catalog_listings get an empty index.
// ponytail: full reload on every governance sync (~10s); fine for hundreds of
// rows. Switch to an updated_at watermark if the table grows large.
func (gs *LocalGovernanceStore) loadModelCards(ctx context.Context) {
	if gs == nil || gs.configStore == nil {
		return
	}
	db := gs.configStore.DB()
	if db == nil {
		return
	}
	db = db.WithContext(ctx)
	if !db.Migrator().HasTable(catalogListingTable) {
		gs.modelCards.Store(&map[string]*modelCard{})
		return
	}

	names, filterStatus := loadPicklistNames(gs, db)
	var rows []catalogRouteRow
	err := db.Raw(`
		SELECT cp.name AS provider_name, cm.name AS model_name, ic.gateway_model_id,
		       ic.context_length::float8 AS context_length, ic.quality_index::float8 AS quality_index,
		       ic.latency_s::float8 AS latency_s, ic.category::text AS category,
		       cl.status::text AS listing_status, ic.status::text AS infra_status,
		       b.ttft_ms::float8 AS ttft_ms, b.throughput_tokens_sec::float8 AS throughput
		FROM catalog_listings cl
		JOIN config_models cm ON cm.id = cl.config_model_id
		JOIN config_providers cp ON cp.id = cl.config_provider_id
		JOIN infra_capabilities ic ON ic.id = cl.infra_capability_id
		LEFT JOIN LATERAL (
			SELECT ttft_ms, throughput_tokens_sec
			FROM capability_benchmarks
			WHERE capability_id = ic.id AND COALESCE(deleted, false) = false
			ORDER BY bench_date DESC NULLS LAST, updated_at DESC
			LIMIT 1
		) b ON true
		WHERE COALESCE(cl.deleted, false) = false
		  AND COALESCE(ic.deleted, false) = false
		  AND ic.gateway_model_id = cm.name`).Scan(&rows).Error
	if err != nil {
		gs.logger.Warn("governance: failed to load catalog listings: %v", err)
		return
	}

	index := make(map[string]*modelCard, len(rows)*2)
	for _, row := range rows {
		if filterStatus {
			if !strings.EqualFold(picklistName(row.ListingStatus, names), "Active") {
				continue
			}
			if !strings.EqualFold(picklistName(row.InfraStatus, names), "Available") {
				continue
			}
		}
		card := row.toCard(names)
		if card == nil {
			continue
		}
		indexCatalogCard(index, card)
	}
	gs.modelCards.Store(&index)
}

func loadPicklistNames(gs *LocalGovernanceStore, db *gorm.DB) (map[string]string, bool) {
	if db == nil || !db.Migrator().HasTable(picklistItemTable) {
		gs.logger.Warn("governance: picklist_item missing; catalog status filter skipped")
		return nil, false
	}
	var rows []struct {
		ID   string
		Name string
	}
	err := db.Raw(`SELECT id::text AS id, name FROM picklist_item WHERE COALESCE(deleted, false) = false`).Scan(&rows).Error
	if err != nil {
		gs.logger.Warn("governance: failed to load picklist names: %v", err)
		return nil, false
	}
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		names[strings.ToLower(strings.TrimSpace(row.ID))] = row.Name
	}
	return names, true
}

func (r catalogRouteRow) toCard(names map[string]string) *modelCard {
	modelName := strings.TrimSpace(deref(r.ModelName))
	gatewayID := strings.TrimSpace(deref(r.GatewayModelID))
	if modelName == "" || gatewayID != modelName {
		return nil
	}
	card := &modelCard{
		Name:       modelName,
		ExternalID: modelName,
		Provider:   strings.TrimSpace(deref(r.ProviderName)),
	}
	if r.ContextLength != nil {
		card.ContextWindow = int(*r.ContextLength)
	}
	if r.QualityIndex != nil {
		card.QualityIndex = *r.QualityIndex
	}
	if r.TTFTMs != nil && *r.TTFTMs > 0 {
		card.TTFTP50Ms = *r.TTFTMs
		card.benchmarked = true
	} else if r.LatencyS != nil && *r.LatencyS > 0 {
		card.TTFTP50Ms = *r.LatencyS * 1000
	}
	if r.Throughput != nil {
		card.ThroughputTokensSec = *r.Throughput
	}
	card.Capabilities, card.InputModalities, card.OutputModalities = catalogSkills(categoryTokens(deref(r.Category), names), card.ContextWindow)
	return card
}

func indexCatalogCard(index map[string]*modelCard, card *modelCard) {
	key := strings.ToLower(card.ExternalID)
	keys := []string{key}
	if card.Provider != "" {
		keys = append([]string{strings.ToLower(card.Provider) + "/" + key}, keys...)
	}
	for _, k := range keys {
		index[k] = preferCatalogCard(index[k], card)
	}
}

// preferCatalogCard keeps the listing that has a measured benchmark, then the
// higher throughput. The first row wins when those are equal.
func preferCatalogCard(current, next *modelCard) *modelCard {
	if current == nil {
		return next
	}
	if next == nil {
		return current
	}
	if next.benchmarked != current.benchmarked {
		if next.benchmarked {
			return next
		}
		return current
	}
	if next.ThroughputTokensSec > current.ThroughputTokensSec {
		return next
	}
	return current
}

func picklistName(raw *string, names map[string]string) string {
	value := strings.Trim(strings.TrimSpace(deref(raw)), `"`)
	if value == "" {
		return ""
	}
	if name, ok := names[strings.ToLower(value)]; ok {
		return name
	}
	return value
}

// categoryTokens resolves a picklist id, JSON array, or postgres uuid array
// to display names, then splits each name on commas.
func categoryTokens(raw string, names map[string]string) []string {
	var tokens []string
	for _, part := range categoryParts(raw) {
		label := strings.TrimSpace(part)
		if name, ok := names[strings.ToLower(label)]; ok {
			label = name
		}
		for _, token := range strings.Split(label, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}

func categoryParts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if json.Unmarshal([]byte(raw), &arr) == nil {
			return arr
		}
	}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		inner := strings.Trim(raw, "{}")
		if inner == "" {
			return nil
		}
		return splitTrim(inner)
	}
	return []string{raw}
}

func splitTrim(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// catalogSkills maps infra category tokens onto the capability names decisions
// score, plus the input and output modalities those tokens imply.
func catalogSkills(tokens []string, contextLength int) (caps, inputs, outputs []string) {
	inputs = []string{"text"}
	outputs = []string{"text"}
	seen := make(map[string]struct{})
	add := func(names ...string) {
		for _, name := range names {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			caps = append(caps, name)
		}
	}
	image := false
	dropText := false
	for _, raw := range tokens {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "reasoning", "flagship", "distilled", "reference precision", "efficient reasoning":
			add("reasoning", "analysis", "mathematics", "architecture", "coding", "code_generation", "debugging", "code_review", "software_engineering")
		case "long context":
			add("long_context", "context_understanding")
		case "general purpose", "workhorse", "cost optimised":
			add("general_knowledge", "instruction_following", "conversation", "summarisation", "factual_qa")
		case "small", "high throughput", "low latency":
			add("low_complexity", "summarisation")
		case "multimodal":
			add("visual_reasoning", "multimodal_reasoning", "image_understanding", "video_understanding")
			image = true
		case "document understanding":
			add("analysis", "context_understanding", "image_understanding")
			image = true
		case "embeddings", "fine-tuned classifier":
			dropText = true
		}
	}
	if contextLength >= longContextTokenFloor {
		add("long_context", "context_understanding")
	}
	if image {
		inputs = append(inputs, "image")
	}
	if dropText {
		outputs = []string{"embeddings"}
	}
	return caps, inputs, outputs
}

// ModelCard returns the tenant card for provider/model, or nil.
func (gs *LocalGovernanceStore) ModelCard(provider schemas.ModelProvider, model string) *modelCard {
	if gs == nil {
		return nil
	}
	index := gs.modelCards.Load()
	if index == nil {
		return nil
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if card, ok := (*index)[strings.ToLower(string(provider))+"/"+model]; ok {
		return card
	}
	return (*index)[model]
}

type modelCardSource interface {
	ModelCard(provider schemas.ModelProvider, model string) *modelCard
}

// rankByModelCards turns a Layer 2 capability profile into an ordered VK pool:
// candidates must have a card that satisfies the profile's hard requirements
// and at least one preferred capability; survivors are ordered by the profile
// objectives (capability fit, quality, latency, and the existing cost score).
// An empty result means no VK model matches and the caller keeps the requested model.
func (p *GovernancePlugin) rankByModelCards(ctx *schemas.BifrostContext, comp *tenantGovernanceComponents, pool []routeCandidate, reqProfile *RequestProfile, cfg *SemanticRoutingConfig, preferenceOverride []string, route *vllmsrRoute) []routeCandidate {
	var cards modelCardSource
	if comp != nil {
		cards, _ = comp.store.(modelCardSource)
	}
	if cards == nil {
		p.logSemantic(ctx, schemas.LogLevelWarn, "2/cards: model card store unavailable for this tenant")
		return nil
	}

	// Existing ranking supplies the cost score and deterministic tie order.
	ranked := p.rankCandidates(pool, reqProfile, cfg, preferenceOverride)

	required := requiredInputModalities(route.Profile, reqProfile)
	rejections := make(map[string][]string, 4)
	matched := make([]routeCandidate, 0, len(ranked))
	for _, candidate := range ranked {
		card := cards.ModelCard(candidate.Provider, candidate.Model)
		if card == nil {
			rejections["no model card"] = append(rejections["no model card"], candidate.qualified())
			continue
		}
		fit, reason := cardMatchesProfile(card, route.Profile, reqProfile, required)
		if reason != "" {
			rejections[reason] = append(rejections[reason], candidate.qualified())
			continue
		}
		candidate.card = card
		candidate.capabilityFit = fit
		matched = append(matched, candidate)
	}
	p.logSemantic(ctx, schemas.LogLevelInfo, "2/cards: decision=%q %d/%d VK models matched profile%s",
		route.Decision, len(matched), len(ranked), describeRejections(rejections))
	if len(matched) == 0 {
		return nil
	}

	scoreByObjectives(matched, route.Profile)
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].score != matched[j].score {
			return matched[i].score > matched[j].score
		}
		return matched[i].card.ThroughputTokensSec > matched[j].card.ThroughputTokensSec
	})
	p.logSemantic(ctx, schemas.LogLevelInfo, "2/cards: ranked %s", describeCardRanking(matched))
	return matched
}

// cardMatchesProfile applies the profile's hard requirements to a card and
// returns its capability fit (0..1). A non-empty reason means rejected.
func cardMatchesProfile(card *modelCard, profile *semanticrouter.CapabilityProfile, reqProfile *RequestProfile, required []string) (float64, string) {
	// Semantic routing only rewrites text-generating requests.
	if len(card.OutputModalities) > 0 && !containsFold(card.OutputModalities, "text") {
		return 0, "no text output"
	}
	inputs := card.InputModalities
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	for _, modality := range required {
		if !containsFold(inputs, modality) {
			return 0, "no " + modality + " input"
		}
	}

	if profile.Context.MinTokensFromRequest || reqProfile.EstInputTokens > unknownLimitTokenCeiling {
		needed := int(float64(reqProfile.EstInputTokens) * (1 + contextSafetyMargin))
		switch {
		case card.ContextWindow > 0 && needed > card.ContextWindow:
			return 0, fmt.Sprintf("context %d < ~%d tokens", card.ContextWindow, needed)
		case card.ContextWindow == 0 && reqProfile.EstInputTokens > unknownLimitTokenCeiling:
			return 0, "unknown context window"
		}
	}

	total, hit := 0.0, 0.0
	for capability, weight := range profile.Prefer.Capabilities {
		if weight <= 0 {
			continue
		}
		total += weight
		if containsFold(card.Capabilities, capability) {
			hit += weight
		}
	}
	if total == 0 {
		return 1, ""
	}
	if hit == 0 {
		return 0, "no preferred capability"
	}
	return hit / total, ""
}

// requiredInputModalities merges the profile's static modalities with the
// modalities present on the request when modality_from_request is set.
func requiredInputModalities(profile *semanticrouter.CapabilityProfile, reqProfile *RequestProfile) []string {
	out := append([]string(nil), profile.Require.InputModalities...)
	if profile.Require.ModalityFromRequest {
		if reqProfile.HasImage {
			out = append(out, "image")
		}
		if reqProfile.HasAudio {
			out = append(out, "audio")
		}
	}
	return out
}

// scoreByObjectives sets score = fit·w_fit + quality·w_q + latency·w_l + cost·w_c.
// Cost reuses rankCandidates' normalized cost score unchanged.
func scoreByObjectives(candidates []routeCandidate, profile *semanticrouter.CapabilityProfile) {
	o := profile.Objectives
	wFit, wQuality, wLatency, wCost := o.CapabilityFit, o.Quality, o.Latency, o.Cost
	if wFit+wQuality+wLatency+wCost == 0 {
		wFit = 1
	}

	minTTFT, maxTTFT := 0.0, 0.0
	for _, c := range candidates {
		if t := c.card.TTFTP50Ms; t > 0 {
			if minTTFT == 0 || t < minTTFT {
				minTTFT = t
			}
			maxTTFT = max(maxTTFT, t)
		}
	}

	for i := range candidates {
		card := candidates[i].card
		quality := 0.5
		if card.QualityIndex > 0 {
			quality = min(card.QualityIndex/10, 1)
		}
		latency := 0.5
		if card.TTFTP50Ms > 0 {
			latency = 1
			if maxTTFT > minTTFT {
				latency = 1 - (card.TTFTP50Ms-minTTFT)/(maxTTFT-minTTFT)
			}
		}
		candidates[i].score = wFit*candidates[i].capabilityFit +
			wQuality*quality +
			wLatency*latency +
			wCost*candidates[i].costScore
	}
}

// cardSatisfiesRequest is the post-route check for models the embedded
// catalog does not know; the card already passed the profile requirements.
func cardSatisfiesRequest(card *modelCard, reqProfile *RequestProfile) (bool, string) {
	inputs := card.InputModalities
	if reqProfile.HasImage && !containsFold(inputs, "image") {
		return false, "no image input support"
	}
	if reqProfile.HasAudio && !containsFold(inputs, "audio") {
		return false, "no audio input support"
	}
	return true, ""
}

func describeCardRanking(ranked []routeCandidate) string {
	limit := min(5, len(ranked))
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		parts = append(parts, fmt.Sprintf("%s=%.3f(fit=%.2f cost=%.2f)",
			ranked[i].qualified(), ranked[i].score, ranked[i].capabilityFit, ranked[i].costScore))
	}
	out := strings.Join(parts, ", ")
	if len(ranked) > limit {
		out += ", ..."
	}
	return out
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
