package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// llmJudgeConfig names the model the judge asks. The call is made with the requesting
// caller's own virtual key, so nothing key-related is stored on the policy.
type llmJudgeConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Prompt   string `json:"prompt"`
}

type llmJudgeEvaluator struct {
	client         *http.Client
	baseURL        string
	internalSecret string
}

func newLLMJudgeEvaluator(baseURL, internalSecret string) *llmJudgeEvaluator {
	return &llmJudgeEvaluator{
		client:         &http.Client{Timeout: 30 * time.Second},
		baseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		internalSecret: internalSecret,
	}
}

func (e *llmJudgeEvaluator) Type() string { return "llm_judge" }

func (e *llmJudgeEvaluator) Info() TypeInfo {
	return typeInfo("llm_judge", "Ask a model, through this gateway, whether the text violates a written rule.", bothStages(), []SettingField{
		{Name: "provider", Type: "string", Description: "Provider", Required: true},
		{Name: "model", Type: "string", Description: "Model", Required: true},
		{Name: "prompt", Type: "string", Description: "Rule the judge applies. It must reply ALLOW or DENY.", Required: true},
	})
}

func (e *llmJudgeEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config llmJudgeConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("llm_judge config: %w", err)
	}
	if e.baseURL == "" {
		return Evaluation{}, fmt.Errorf("AI_GATEWAY_BASE_URL is not set on the gateway")
	}
	token := callerToken(ctx)
	if token == "" {
		return Evaluation{}, fmt.Errorf("llm_judge calls a model as the requesting virtual key, but this request has no bearer token")
	}
	if strings.TrimSpace(config.Provider) == "" || strings.TrimSpace(config.Model) == "" || strings.TrimSpace(config.Prompt) == "" {
		return Evaluation{}, fmt.Errorf("llm_judge config requires provider, model, and prompt")
	}
	body := map[string]any{
		"model": gatewayModelName(config.Provider, config.Model),
		"messages": []map[string]string{
			{"role": "system", "content": config.Prompt + " Reply with ALLOW or DENY."},
			{"role": "user", "content": joinMessageText(input.Messages)},
		},
	}
	endpoint := e.baseURL + "/v1/chat/completions"
	raw, err := postJSON(ctx, e.client, endpoint, body, token, map[string]string{
		"x-m360-guardrail-internal": e.internalSecret,
	})
	if err != nil {
		return Evaluation{}, err
	}
	verdict := judgeText(raw)
	denied := strings.HasPrefix(verdict, "DENY") || strings.HasPrefix(verdict, "BLOCK")
	return Evaluation{Matched: denied, Reasons: []string{verdict}}, nil
}

func judgeText(raw []byte) string {
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Choices) == 0 {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(body.Choices[0].Message.Content))
}

func joinMessageText(messages []MessageText) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if strings.TrimSpace(message.Text) == "" {
			continue
		}
		parts = append(parts, message.Text)
	}
	return strings.Join(parts, "\n")
}
