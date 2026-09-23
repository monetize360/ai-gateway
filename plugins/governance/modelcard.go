package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticrouter"
)

const (
	modelCardTable = "model_card"

	// modelCardToolCapability is the card capability that satisfies a
	// conditional tool_calling requirement from the capability profile.
	modelCardToolCapability = "tool_calling"
)

// modelCard is the routing view of one tenant model_card row.
type modelCard struct {
	Name             string
	Provider         string
	ExternalID       string
	InputModalities  []string
	OutputModalities []string
	ContextWindow    int
	Capabilities     []string
	QualityIndex     float64 // 0..10; 0 when unknown
	TTFTP50Ms        float64 // 0 when unknown
}

type modelCardRow struct {
	Name             string
	ExternalID       *string
	InputModalities  *string
	OutputModalities *string
	ContextWindow    *float64
	Capabilities     *string
	RawMetadata      *string
	ProviderName     *string
}

// loadModelCards replaces the tenant's model card index from model_card.
// Tenants without the table get an empty index.
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
	if !db.Migrator().HasTable(modelCardTable) {
		gs.modelCards.Store(&map[string]*modelCard{})
		return
	}

	var rows []modelCardRow
	err := db.Raw(`
		SELECT mc.name, mc.external_id, mc.input_modalities::text AS input_modalities,
		       mc.output_modalities::text AS output_modalities, mc.context_window::float8 AS context_window,
		       mc.capabilities::text AS capabilities, mc.raw_metadata::text AS raw_metadata,
		       cp.name AS provider_name
		FROM model_card mc
		LEFT JOIN config_providers cp ON cp.id = mc.created_by_provider_id
		WHERE COALESCE(mc.deleted, false) = false`).Scan(&rows).Error
	if err != nil {
		gs.logger.Warn("governance: failed to load model cards: %v", err)
		return
	}

	index := make(map[string]*modelCard, len(rows)*2)
	for _, row := range rows {
		card := row.toCard()
		if card.ExternalID == "" {
			continue
		}
		key := strings.ToLower(card.ExternalID)
		if card.Provider != "" {
			index[strings.ToLower(card.Provider)+"/"+key] = card
		}
		if _, taken := index[key]; !taken {
			index[key] = card
		}
	}
	gs.modelCards.Store(&index)
}

func (r modelCardRow) toCard() *modelCard {
	card := &modelCard{
		Name:             r.Name,
		ExternalID:       strings.TrimSpace(deref(r.ExternalID)),
		Provider:         strings.TrimSpace(deref(r.ProviderName)),
		InputModalities:  jsonStrings(r.InputModalities),
		OutputModalities: jsonStrings(r.OutputModalities),
		Capabilities:     jsonStrings(r.Capabilities),
	}
	if r.ContextWindow != nil {
		card.ContextWindow = int(*r.ContextWindow)
	}
	if r.RawMetadata != nil {
		var meta struct {
			QualityIndex float64 `json:"quality_index"`
			TTFTP50Ms    float64 `json:"ttft_p50_ms"`
		}
		if json.Unmarshal([]byte(*r.RawMetadata), &meta) == nil {
			card.QualityIndex = meta.QualityIndex
			card.TTFTP50Ms = meta.TTFTP50Ms
		}
	}
	return card
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
		fit, reason := cardMatchesProfile(card, route.Profile, route.Signals, reqProfile, required)
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
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].score > matched[j].score })
	p.logSemantic(ctx, schemas.LogLevelInfo, "2/cards: ranked %s", describeCardRanking(matched))
	return matched
}

// cardMatchesProfile applies the profile's hard requirements to a card and
// returns its capability fit (0..1). A non-empty reason means rejected.
func cardMatchesProfile(card *modelCard, profile *semanticrouter.CapabilityProfile, signals *semanticrouter.MatchedSignals, reqProfile *RequestProfile, required []string) (float64, string) {
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

	needsTools := reqProfile.HasTools
	for _, cond := range profile.Require.Conditional {
		if cond.Require.ToolCalling != nil && *cond.Require.ToolCalling && signalMatched(signals, cond.When.Type, cond.When.Name) {
			needsTools = true
		}
	}
	if needsTools && !containsFold(card.Capabilities, modelCardToolCapability) {
		return 0, "no tool calling"
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

func signalMatched(signals *semanticrouter.MatchedSignals, kind, name string) bool {
	if signals == nil {
		return false
	}
	var names []string
	switch strings.ToLower(kind) {
	case "keyword":
		names = signals.Keywords
	case "structure":
		names = signals.Structure
	case "conversation":
		names = signals.Conversation
	case "context":
		names = signals.Context
	case "language":
		names = signals.Language
	case "input_modality":
		names = signals.InputModality
	case "domain":
		names = signals.Domains
	case "complexity":
		names = signals.Complexity
	}
	return slices.Contains(names, name)
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
	if reqProfile.HasTools && !containsFold(card.Capabilities, modelCardToolCapability) {
		return false, "no tool calling support"
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

func jsonStrings(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(*raw), &out)
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
