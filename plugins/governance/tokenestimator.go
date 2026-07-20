package governance

// estimateInputTokenCount estimates the number of input tokens in a request payload
// using a character-based heuristic (total characters / 4).
// It extracts text from common LLM request fields: messages, system, prompt, and input.
// Returns 0 if no text content is found.
func estimateInputTokenCount(payload map[string]any) int {
	if len(payload) == 0 {
		return 0
	}

	totalChars := 0

	// Extract from "messages" array (chat completions — OpenAI, Anthropic, etc.)
	if messages, ok := payload["messages"].([]any); ok {
		for _, msg := range messages {
			msgMap, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			totalChars += extractContentLength(msgMap["content"])
		}
	}

	// Extract from "system" field (top-level system prompt — Anthropic format or OpenAI string)
	totalChars += extractContentLength(payload["system"])

	// Extract from "prompt" field (text completions)
	totalChars += extractContentLength(payload["prompt"])

	// Extract from "input" field (embeddings, responses API)
	totalChars += extractContentLength(payload["input"])

	return totalChars / 4
}

// extractContentLength returns the character count of text content.
// Handles three formats:
//   - string: returns len(s)
//   - []any of {"type":"text","text":"..."} parts: sums text field lengths
//   - []any of strings: sums all string lengths
func extractContentLength(content any) int {
	if content == nil {
		return 0
	}

	switch v := content.(type) {
	case string:
		return len(v)

	case []any:
		total := 0
		for _, item := range v {
			switch part := item.(type) {
			case string:
				total += len(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					total += len(text)
				}
			}
		}
		return total

	default:
		return 0
	}
}
