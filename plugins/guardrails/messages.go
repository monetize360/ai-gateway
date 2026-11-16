package guardrails

import (
	"encoding/json"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// redactedToolArguments replaces tool-call arguments that cannot be redacted in place.
// It stays valid JSON so providers still accept the tool call.
const redactedToolArguments = "{}"

type textPartKind int

const (
	contentTextPart textPartKind = iota
	toolArgumentsTextPart
)

// textPart is one piece of message text and the location it is written back to.
type textPart struct {
	kind     textPartKind
	original string
	text     string
	write    func(string)
}

// MessageText is one message the evaluators see. Text joins the message's text parts.
type MessageText struct {
	Role  string `json:"role"`
	Text  string `json:"text"`
	parts []textPart
}

func newTextPart(kind textPartKind, text string, write func(string)) textPart {
	return textPart{kind: kind, original: text, text: text, write: write}
}

func newMessageText(role string, parts []textPart) MessageText {
	return MessageText{Role: role, Text: joinTextParts(parts), parts: parts}
}

func joinTextParts(parts []textPart) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.text != "" {
			texts = append(texts, part.text)
		}
	}
	return strings.Join(texts, "\n")
}

// mapParts returns a copy with every text part rewritten by transform.
// Messages built without parts (test API, stream buffer) rewrite Text as one content part.
func (m MessageText) mapParts(transform func(part textPart) string) MessageText {
	if len(m.parts) == 0 {
		m.Text = transform(textPart{kind: contentTextPart, original: m.Text, text: m.Text})
		return m
	}
	parts := make([]textPart, len(m.parts))
	for i, part := range m.parts {
		part.text = transform(part)
		parts[i] = part
	}
	m.parts = parts
	m.Text = joinTextParts(parts)
	return m
}

// replaceText writes a full replacement of the joined text. The replacement cannot be split back
// across parts, so it lands on the first content part and every other part is cleared.
// A tool-call argument only keeps that text when it is still valid JSON.
func (m MessageText) replaceText(replacement string) MessageText {
	if len(m.parts) <= 1 {
		return m.mapParts(func(part textPart) string {
			if part.kind == toolArgumentsTextPart {
				return keepToolArguments(replacement)
			}
			return replacement
		})
	}
	written := false
	return m.mapParts(func(part textPart) string {
		if part.kind == toolArgumentsTextPart {
			return redactedToolArguments
		}
		if written {
			return ""
		}
		written = true
		return replacement
	})
}

// keepToolArguments returns text when it is valid JSON, otherwise an empty object.
func keepToolArguments(text string) string {
	if json.Valid([]byte(text)) {
		return text
	}
	return redactedToolArguments
}

// redactAll overwrites every text part with the placeholder.
func (m MessageText) redactAll(placeholder string) MessageText {
	return m.mapParts(func(part textPart) string {
		if part.kind == toolArgumentsTextPart {
			return redactedToolArguments
		}
		return placeholder
	})
}

// applyMessages writes changed text parts back to the request or response they were read from.
func applyMessages(messages []MessageText) {
	for _, message := range messages {
		for _, part := range message.parts {
			if part.write != nil && part.text != part.original {
				part.write(part.text)
			}
		}
	}
}

func chatContentParts(content *schemas.ChatMessageContent) []textPart {
	if content == nil {
		return nil
	}
	if content.ContentStr != nil {
		if *content.ContentStr == "" {
			return nil
		}
		return []textPart{newTextPart(contentTextPart, *content.ContentStr, func(text string) { content.ContentStr = &text })}
	}
	var parts []textPart
	for i := range content.ContentBlocks {
		block := &content.ContentBlocks[i]
		if block.Text == nil || *block.Text == "" {
			continue
		}
		parts = append(parts, newTextPart(contentTextPart, *block.Text, func(text string) { block.Text = &text }))
	}
	return parts
}

func responsesContentParts(content *schemas.ResponsesMessageContent) []textPart {
	if content == nil {
		return nil
	}
	if content.ContentStr != nil {
		if *content.ContentStr == "" {
			return nil
		}
		return []textPart{newTextPart(contentTextPart, *content.ContentStr, func(text string) { content.ContentStr = &text })}
	}
	return responsesBlockParts(content.ContentBlocks)
}

func responsesBlockParts(blocks []schemas.ResponsesMessageContentBlock) []textPart {
	var parts []textPart
	for i := range blocks {
		block := &blocks[i]
		if block.Text == nil || *block.Text == "" {
			continue
		}
		parts = append(parts, newTextPart(contentTextPart, *block.Text, func(text string) { block.Text = &text }))
	}
	return parts
}

// requestMessages returns the request's messages, including tool-call arguments, for chat,
// responses, and text completion requests.
func requestMessages(req *schemas.BifrostRequest) []MessageText {
	if req == nil {
		return nil
	}
	switch {
	case req.ChatRequest != nil:
		return chatRequestMessages(req.ChatRequest)
	case req.ResponsesRequest != nil:
		return responsesRequestMessages(req.ResponsesRequest)
	case req.TextCompletionRequest != nil:
		return textCompletionRequestMessages(req.TextCompletionRequest)
	}
	return nil
}

func chatRequestMessages(chatRequest *schemas.BifrostChatRequest) []MessageText {
	out := make([]MessageText, 0, len(chatRequest.Input))
	for i := range chatRequest.Input {
		message := &chatRequest.Input[i]
		parts := chatContentParts(message.Content)
		if message.ChatAssistantMessage != nil {
			for j := range message.ChatAssistantMessage.ToolCalls {
				call := &message.ChatAssistantMessage.ToolCalls[j]
				if call.Function.Arguments == "" {
					continue
				}
				parts = append(parts, newTextPart(toolArgumentsTextPart, call.Function.Arguments, func(text string) { call.Function.Arguments = text }))
			}
		}
		out = append(out, newMessageText(string(message.Role), parts))
	}
	return out
}

func responsesRequestMessages(responsesRequest *schemas.BifrostResponsesRequest) []MessageText {
	out := make([]MessageText, 0, len(responsesRequest.Input)+1)
	if params := responsesRequest.Params; params != nil && params.Instructions != nil && *params.Instructions != "" {
		out = append(out, newMessageText("system", []textPart{
			newTextPart(contentTextPart, *params.Instructions, func(text string) { params.Instructions = &text }),
		}))
	}
	for i := range responsesRequest.Input {
		item := &responsesRequest.Input[i]
		parts := responsesContentParts(item.Content)
		if toolMessage := item.ResponsesToolMessage; toolMessage != nil {
			if toolMessage.Arguments != nil && *toolMessage.Arguments != "" {
				parts = append(parts, newTextPart(toolArgumentsTextPart, *toolMessage.Arguments, func(text string) { toolMessage.Arguments = &text }))
			}
			if output := toolMessage.Output; output != nil {
				if output.ResponsesToolCallOutputStr != nil && *output.ResponsesToolCallOutputStr != "" {
					parts = append(parts, newTextPart(contentTextPart, *output.ResponsesToolCallOutputStr, func(text string) { output.ResponsesToolCallOutputStr = &text }))
				}
				parts = append(parts, responsesBlockParts(output.ResponsesFunctionToolCallOutputBlocks)...)
			}
		}
		out = append(out, newMessageText(responsesItemRole(item), parts))
	}
	return out
}

func responsesItemRole(item *schemas.ResponsesMessage) string {
	if item.Role != nil && *item.Role != "" {
		return string(*item.Role)
	}
	if item.Type != nil {
		switch *item.Type {
		case schemas.ResponsesMessageTypeFunctionCallOutput, schemas.ResponsesMessageTypeCustomToolCallOutput:
			return "tool"
		case schemas.ResponsesMessageTypeMessage:
			return "user"
		}
		return "assistant"
	}
	return "user"
}

func textCompletionRequestMessages(textRequest *schemas.BifrostTextCompletionRequest) []MessageText {
	input := textRequest.Input
	if input == nil {
		return nil
	}
	var parts []textPart
	if input.PromptStr != nil && *input.PromptStr != "" {
		parts = append(parts, newTextPart(contentTextPart, *input.PromptStr, func(text string) { input.PromptStr = &text }))
	}
	for i := range input.PromptArray {
		index := i
		if input.PromptArray[index] == "" {
			continue
		}
		parts = append(parts, newTextPart(contentTextPart, input.PromptArray[index], func(text string) { input.PromptArray[index] = text }))
	}
	return []MessageText{newMessageText("user", parts)}
}

func trimMessages(messages []MessageText, maxTurns int) []MessageText {
	if maxTurns <= 0 || len(messages) <= maxTurns {
		return messages
	}
	return messages[len(messages)-maxTurns:]
}

// responseMessages returns the non-stream response text as one assistant message.
func responseMessages(resp *schemas.BifrostResponse) []MessageText {
	if resp == nil {
		return nil
	}
	var parts []textPart
	switch {
	case resp.ChatResponse != nil:
		for _, choice := range resp.ChatResponse.Choices {
			if choice.ChatNonStreamResponseChoice == nil || choice.ChatNonStreamResponseChoice.Message == nil {
				continue
			}
			parts = append(parts, chatContentParts(choice.ChatNonStreamResponseChoice.Message.Content)...)
		}
	case resp.ResponsesResponse != nil:
		for i := range resp.ResponsesResponse.Output {
			parts = append(parts, responsesContentParts(resp.ResponsesResponse.Output[i].Content)...)
		}
	case resp.TextCompletionResponse != nil:
		for _, choice := range resp.TextCompletionResponse.Choices {
			textChoice := choice.TextCompletionResponseChoice
			if textChoice == nil || textChoice.Text == nil || *textChoice.Text == "" {
				continue
			}
			parts = append(parts, newTextPart(contentTextPart, *textChoice.Text, func(text string) { textChoice.Text = &text }))
		}
	default:
		return nil
	}
	return []MessageText{newMessageText("assistant", parts)}
}

// responseModel returns the provider and model a non-stream response came from.
func responseModel(resp *schemas.BifrostResponse) (schemas.ModelProvider, string) {
	var provider schemas.ModelProvider
	if extra := resp.GetExtraFields(); extra != nil {
		provider = extra.Provider
	}
	switch {
	case resp.ChatResponse != nil:
		return provider, resp.ChatResponse.Model
	case resp.ResponsesResponse != nil:
		return provider, resp.ResponsesResponse.Model
	case resp.TextCompletionResponse != nil:
		return provider, resp.TextCompletionResponse.Model
	}
	return provider, ""
}

func chunkDelta(chunk *schemas.BifrostStreamChunk) (text string, delta **string, finish bool) {
	if chunk == nil || chunk.BifrostChatResponse == nil {
		return "", nil, false
	}
	for _, choice := range chunk.BifrostChatResponse.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finish = true
		}
		if choice.ChatStreamResponseChoice == nil || choice.ChatStreamResponseChoice.Delta == nil {
			continue
		}
		if choice.ChatStreamResponseChoice.Delta.Content != nil {
			return *choice.ChatStreamResponseChoice.Delta.Content, &choice.ChatStreamResponseChoice.Delta.Content, finish
		}
	}
	return "", nil, finish
}
