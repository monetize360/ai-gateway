package guardrails

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// MessageText is one chat message the evaluators see.
type MessageText struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func contentString(content *schemas.ChatMessageContent) string {
	if content == nil || content.ContentStr == nil {
		return ""
	}
	return *content.ContentStr
}

func setContentString(content **schemas.ChatMessageContent, text string) {
	if *content == nil {
		*content = &schemas.ChatMessageContent{}
	}
	(*content).ContentStr = &text
	(*content).ContentBlocks = nil
}

// requestMessages returns chat messages, including tool-call arguments.
func requestMessages(req *schemas.BifrostRequest) []MessageText {
	if req == nil || req.ChatRequest == nil {
		return nil
	}
	out := make([]MessageText, 0, len(req.ChatRequest.Input))
	for _, message := range req.ChatRequest.Input {
		text := contentString(message.Content)
		if message.ChatAssistantMessage != nil {
			for _, call := range message.ChatAssistantMessage.ToolCalls {
				if call.Function.Arguments != "" {
					if text != "" {
						text += "\n"
					}
					text += call.Function.Arguments
				}
			}
		}
		out = append(out, MessageText{Role: string(message.Role), Text: text})
	}
	return out
}

func trimMessages(messages []MessageText, maxTurns int) []MessageText {
	if maxTurns <= 0 || len(messages) <= maxTurns {
		return messages
	}
	return messages[len(messages)-maxTurns:]
}

func applyRequestMessages(req *schemas.BifrostRequest, messages []MessageText) {
	if req == nil || req.ChatRequest == nil {
		return
	}
	input := req.ChatRequest.Input
	if len(messages) > len(input) {
		return
	}
	start := len(input) - len(messages)
	for i, message := range messages {
		target := &input[start+i]
		if contentString(target.Content) == message.Text {
			continue
		}
		setContentString(&target.Content, message.Text)
	}
}

func responseText(resp *schemas.BifrostResponse) string {
	if resp == nil || resp.ChatResponse == nil {
		return ""
	}
	var parts []string
	for _, choice := range resp.ChatResponse.Choices {
		if choice.ChatNonStreamResponseChoice == nil || choice.ChatNonStreamResponseChoice.Message == nil {
			continue
		}
		if text := contentString(choice.ChatNonStreamResponseChoice.Message.Content); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func applyResponseText(resp *schemas.BifrostResponse, text string) {
	if resp == nil || resp.ChatResponse == nil {
		return
	}
	for _, choice := range resp.ChatResponse.Choices {
		if choice.ChatNonStreamResponseChoice == nil || choice.ChatNonStreamResponseChoice.Message == nil {
			continue
		}
		setContentString(&choice.ChatNonStreamResponseChoice.Message.Content, text)
		return
	}
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
