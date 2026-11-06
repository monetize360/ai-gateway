package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type lakeraConfig struct {
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	ProjectID string `json:"project_id"`
}

type lakeraEvaluator struct {
	client *http.Client
}

func newLakeraEvaluator() *lakeraEvaluator {
	return &lakeraEvaluator{client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *lakeraEvaluator) Type() string { return "lakera" }

func (e *lakeraEvaluator) Info() TypeInfo {
	return typeInfo("lakera", "Send the messages to Lakera Guard.", bothStages(), []SettingField{
		{Name: "base_url", Type: "string", Description: "Lakera API base URL", Default: "https://api.lakera.ai"},
		{Name: "api_key_env", Type: "string", Description: "API key or env.VAR_NAME", Required: true},
		{Name: "project_id", Type: "string", Description: "Optional Lakera project id"},
	})
}

func (e *lakeraEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config lakeraConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("lakera config: %w", err)
	}
	base := strings.TrimSpace(config.BaseURL)
	if base == "" {
		base = "https://api.lakera.ai"
	}
	messages := make([]map[string]string, 0, len(input.Messages))
	for _, message := range input.Messages {
		role := message.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]string{"role": role, "content": message.Text})
	}
	body := map[string]any{"messages": messages}
	if strings.TrimSpace(config.ProjectID) != "" {
		body["project_id"] = config.ProjectID
	}
	raw, err := postJSON(ctx, e.client, strings.TrimRight(base, "/")+"/v2/guard", body, resolveSecret(config.APIKeyEnv), nil)
	if err != nil {
		return Evaluation{}, err
	}
	var response struct {
		Flagged bool `json:"flagged"`
		Results []struct {
			Flagged bool `json:"flagged"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Evaluation{}, err
	}
	flagged := response.Flagged
	for _, result := range response.Results {
		if result.Flagged {
			flagged = true
		}
	}
	return Evaluation{Matched: flagged, Reasons: []string{"lakera"}}, nil
}
