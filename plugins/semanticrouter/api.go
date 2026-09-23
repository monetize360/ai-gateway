package semanticrouter

import (
	"context"
	"fmt"

	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/config"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/services"
)

const PluginName = "semantic-router"

// Router is the in-process Layer 2 preview surface called by governance.
type Router interface {
	Preview(ctx context.Context, in PreviewInput) (*PreviewResult, error)
}

// PreviewInput is the OpenAI-chat shaped request body fragment used for signal extraction.
type PreviewInput struct {
	Model               string
	Messages            []map[string]any
	Text                string
	Tools               any
	Functions           any
	ToolChoice          any
	FunctionCall        any
	ResponseFormat      any
	MaxTokens           any
	MaxCompletionTokens any
	Metadata            map[string]string
	PreviewContext      map[string]any
}

// CapabilityProfile is the inventory-independent quality contract Preview
// returns for a winning decision. Governance applies it to the live catalog.
type CapabilityProfile = config.CapabilityProfile

// MatchedSignals names the router signals that fired for the request.
type MatchedSignals = services.MatchedSignals

// PreviewResult mirrors the fields governance already consumes from the HTTP
// vLLM-SR preview API (selected_model, candidates, selection_status, …).
type PreviewResult struct {
	SelectedModel     string
	Decision          string
	Algorithm         string
	SelectionStatus   string
	SelectionMethod   string
	Candidates        []string
	MatchedSignals    *services.MatchedSignals
	CapabilityProfile *CapabilityProfile
}

func (p *PreviewResult) ProfileSummary() string {
	if p == nil || p.CapabilityProfile == nil {
		return "{}"
	}
	profile := p.CapabilityProfile
	return fmt.Sprintf("modalities=%v modality_from_request=%t conditional=%d prefer=%v context_from_request=%t objectives={fit=%.2f quality=%.2f latency=%.2f cost=%.2f}",
		profile.Require.InputModalities,
		profile.Require.ModalityFromRequest,
		len(profile.Require.Conditional),
		profile.Prefer.Capabilities,
		profile.Context.MinTokensFromRequest,
		profile.Objectives.CapabilityFit,
		profile.Objectives.Quality,
		profile.Objectives.Latency,
		profile.Objectives.Cost,
	)
}

func (p *PreviewResult) SignalSummary() string {
	if p == nil || p.MatchedSignals == nil {
		return "{}"
	}
	signals := p.MatchedSignals
	return fmt.Sprintf("keywords=%v structure=%v language=%v conversation=%v context=%v input_modality=%v",
		signals.Keywords, signals.Structure, signals.Language, signals.Conversation, signals.Context, signals.InputModality)
}
