package governance

import (
	"encoding/json"
	"strings"
)

// charsPerTokenEstimate approximates tokens from character counts.
//
// ponytail: naive fixed divisor rather than a real tokenizer. Ceiling: it
// under-counts CJK and code-heavy prompts by roughly 2x, so a request near a
// model's context limit can be judged to fit when it does not. Upgrade path is a
// per-family tokenizer keyed off PricingEntry.Architecture.Tokenizer; until then
// contextSafetyMargin in capability.go absorbs the error.
const charsPerTokenEstimate = 4

var (
	mimeTypeKeys       = []string{"mime_type", "mimeType", "media_type", "mediaType"}
	mimeNestedKeys     = []string{"source", "file", "image_url", "input_audio"}
	geminiBlobKeys     = []string{"inline_data", "inlineData", "file_data", "fileData"}
	topLevelTextFields = []string{"system", "prompt", "input"}
	toolFields         = []string{"tools", "functions"}
	maxTokenFields     = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
)

// RequestProfile is the deterministic analysis of an inbound request payload —
// Layer 1 of semantic routing. Every field is derived from the parsed body alone:
// no network calls, no model inference, no LLM.
type RequestProfile struct {
	// Recognized reports whether any known content field was found. A payload whose
	// shape we cannot read must not be routed on: a missed image part would let an
	// image request reach a text-only model.
	Recognized bool

	InputChars     int
	EstInputTokens int
	MessageCount   int

	// Input modality requirements, derived from content part types and mime types.
	HasImage bool
	HasPDF   bool
	HasAudio bool

	HasTools        bool
	ToolCount       int
	NeedsJSONSchema bool
	IsStreaming     bool

	// RequestedMaxTokens is 0 when the caller did not ask for a specific limit.
	RequestedMaxTokens int
}

// buildRequestProfile walks a parsed request payload once and derives every
// deterministic routing signal from it. It understands the OpenAI chat, Anthropic
// messages, Responses, text-completion, embedding and Gemini body shapes.
func buildRequestProfile(payload map[string]any) *RequestProfile {
	profile := &RequestProfile{}
	if len(payload) == 0 {
		return profile
	}

	profile.inspectEntries(payload, "messages", "content") // OpenAI chat, Anthropic messages
	profile.inspectEntries(payload, "contents", "parts")   // Gemini / GenAI

	// Top-level text fields: system prompt (Anthropic / OpenAI string form),
	// "prompt" (text completions), "input" (embeddings, Responses API).
	for _, field := range topLevelTextFields {
		if value, ok := payload[field]; ok && value != nil {
			profile.Recognized = true
			profile.inspectContent(value)
		}
	}
	if instructions, ok := payload["system_instruction"]; ok && instructions != nil {
		profile.Recognized = true
		profile.inspectContent(instructions)
	}

	profile.EstInputTokens = (profile.InputChars + charsPerTokenEstimate - 1) / charsPerTokenEstimate

	// Tool definitions. "functions" is the legacy OpenAI spelling.
	for _, field := range toolFields {
		if tools, ok := payload[field].([]any); ok && len(tools) > 0 {
			profile.HasTools = true
			profile.ToolCount += len(tools)
		}
	}

	profile.NeedsJSONSchema = needsStructuredOutput(payload)

	if stream, ok := payload["stream"].(bool); ok {
		profile.IsStreaming = stream
	}

	for _, field := range maxTokenFields {
		if n, ok := numberFromPayload(payload[field]); ok && n > profile.RequestedMaxTokens {
			profile.RequestedMaxTokens = n
		}
	}

	return profile
}

// inspectEntries walks a list of message-like objects and inspects one content key
// on each. Missing or non-array fields are a no-op so both OpenAI and Gemini shapes
// can be tried on the same payload.
func (profile *RequestProfile) inspectEntries(payload map[string]any, field, contentKey string) {
	items, ok := payload[field].([]any)
	if !ok {
		return
	}
	profile.Recognized = true
	profile.MessageCount += len(items)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		profile.inspectContent(entry[contentKey])
	}
}

// inspectContent accumulates character counts and modality flags from a content
// value. Handles a bare string, an array of strings, and an array of typed parts
// in OpenAI, Anthropic, Responses or Gemini shape.
func (profile *RequestProfile) inspectContent(content any) {
	if content == nil {
		return
	}

	switch v := content.(type) {
	case string:
		profile.InputChars += len(v)

	case []any:
		for _, item := range v {
			switch part := item.(type) {
			case string:
				profile.InputChars += len(part)
			case map[string]any:
				profile.inspectPart(part)
			}
		}

	case map[string]any:
		// Gemini system_instruction arrives as a single {parts: [...]} object.
		profile.inspectPart(v)
	}
}

// inspectPart reads one structured content part.
func (profile *RequestProfile) inspectPart(part map[string]any) {
	if text, ok := part["text"].(string); ok {
		profile.InputChars += len(text)
	}
	// Gemini nests parts one level deeper.
	if nested, ok := part["parts"]; ok {
		profile.inspectContent(nested)
	}

	if partType, ok := part["type"].(string); ok {
		switch partType {
		case "image_url", "image", "input_image":
			profile.HasImage = true
		case "input_audio", "audio":
			profile.HasAudio = true
		case "file", "document", "input_file":
			// Attachments are treated as PDF unless the mime type says otherwise.
			// Erring strict here filters out models that cannot accept files at all,
			// which is the safe direction.
			profile.applyMimeType(partMimeType(part), true)
		}
	}

	// Gemini inline data and file references carry the mime type instead of a type tag.
	for _, key := range geminiBlobKeys {
		if blob, ok := part[key].(map[string]any); ok {
			profile.applyMimeType(partMimeType(blob), false)
		}
	}
}

// applyMimeType sets the modality flag implied by a mime type. When the mime type
// is absent, defaultPDF decides whether to assume a document attachment.
func (profile *RequestProfile) applyMimeType(mimeType string, defaultPDF bool) {
	switch {
	case mimeType == "":
		profile.HasPDF = profile.HasPDF || defaultPDF
	case strings.HasPrefix(mimeType, "image/"):
		profile.HasImage = true
	case strings.HasPrefix(mimeType, "audio/"):
		profile.HasAudio = true
	case strings.HasPrefix(mimeType, "video/"):
		// No dedicated video capability flag in the catalog; vision is the closest
		// gate and every video-capable model is also vision-capable.
		profile.HasImage = true
	default:
		profile.HasPDF = true
	}
}

// firstStringField returns the first non-empty string at any of the given keys.
func firstStringField(m map[string]any, keys []string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// partMimeType pulls a mime type out of the several spellings providers use, looking
// one level into a nested source/file object when needed.
func partMimeType(part map[string]any) string {
	if mimeType := firstStringField(part, mimeTypeKeys); mimeType != "" {
		return mimeType
	}
	for _, key := range mimeNestedKeys {
		nested, ok := part[key].(map[string]any)
		if !ok {
			continue
		}
		if mimeType := firstStringField(nested, mimeTypeKeys); mimeType != "" {
			return mimeType
		}
	}
	return ""
}

// needsStructuredOutput reports whether the request pins a JSON schema or JSON
// object response, in either the chat completions or Responses API spelling.
func needsStructuredOutput(payload map[string]any) bool {
	if format, ok := payload["response_format"].(map[string]any); ok {
		if formatType, ok := format["type"].(string); ok {
			return formatType == "json_schema" || formatType == "json_object"
		}
		return len(format) > 0
	}
	if text, ok := payload["text"].(map[string]any); ok {
		if _, ok := text["format"]; ok {
			return true
		}
	}
	return false
}

// numberFromPayload reads an integer out of a decoded JSON value, which may be a
// float64, a json.Number or an int depending on the decoder.
func numberFromPayload(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	}
	return 0, false
}

// estimateInputContextLength returns the total character count of text content in a
// request payload. Retained for the routing-rule CEL variable of the same name.
func estimateInputContextLength(payload map[string]any) int {
	return buildRequestProfile(payload).InputChars
}
