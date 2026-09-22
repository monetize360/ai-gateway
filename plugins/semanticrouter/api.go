package semanticrouter

import "context"

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

// PreviewResult mirrors the fields governance already consumes from the HTTP
// vLLM-SR preview API (selected_model, candidates, selection_status, …).
type PreviewResult struct {
	SelectedModel   string
	Decision        string
	Algorithm       string
	SelectionStatus string
	SelectionMethod string
	Candidates      []string
}
