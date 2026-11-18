package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type webhookConfig struct {
	URL       string `json:"url"`
	BearerEnv string `json:"bearer_env"`
}

type webhookEvaluator struct {
	client *http.Client
}

func newWebhookEvaluator() *webhookEvaluator {
	return &webhookEvaluator{client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *webhookEvaluator) Type() string { return "webhook" }

func (e *webhookEvaluator) Info() TypeInfo {
	return typeInfo("webhook", "POST the messages to a service you run. A match applies the policy action.", bothStages(), []SettingField{
		{Name: "url", Type: "string", Description: "HTTPS endpoint that receives the check", Required: true},
		{Name: "bearer_env", Type: "string", Description: "Bearer token or env.VAR_NAME reference"},
	})
}

func (e *webhookEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config webhookConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("webhook config: %w", err)
	}
	if strings.TrimSpace(config.URL) == "" {
		return Evaluation{}, fmt.Errorf("webhook config requires url")
	}
	body := map[string]any{
		"stage":       input.Stage,
		"policy_name": policy.Name,
		"messages":    input.Messages,
	}
	raw, err := postJSON(ctx, e.client, config.URL, body, resolveSecret(config.BearerEnv), nil)
	if err != nil {
		return Evaluation{}, err
	}
	var response struct {
		Matched      bool     `json:"matched"`
		Reasons      []string `json:"reasons"`
		RedactedText string   `json:"redacted_text"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Evaluation{}, fmt.Errorf("webhook response: %w", err)
	}
	return Evaluation{Matched: response.Matched, Reasons: response.Reasons, Replacement: response.RedactedText}, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body any, bearer string, headers map[string]string) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return raw, nil
}
