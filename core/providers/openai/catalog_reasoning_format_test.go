package openai

import (
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCustomOpenAINoneReasoningEffortIsSent(t *testing.T) {
	ctx, cancel := schemas.NewBifrostContextWithCancel(nil)
	defer cancel()

	req := &schemas.BifrostChatRequest{
		Provider: schemas.ModelProvider("acme-vllm"),
		Model:    "qwen3",
		Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
		Params: &schemas.ChatParameters{
			Reasoning: &schemas.ChatReasoning{Effort: schemas.Ptr("none")},
		},
	}
	converted := ToOpenAIChatRequest(ctx, req)
	require.NotNil(t, converted)
	raw, err := json.Marshal(converted)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "none", body["reasoning_effort"])
	_, hasKwargs := body["chat_template_kwargs"]
	require.False(t, hasKwargs)
}
