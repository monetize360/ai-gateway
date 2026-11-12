package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type azureConfig struct {
	Endpoint          string   `json:"endpoint"`
	KeyEnv            string   `json:"key_env"`
	Categories        []string `json:"categories"`
	SeverityThreshold int      `json:"severity_threshold"`
}

type azureEvaluator struct {
	client *http.Client
}

func newAzureEvaluator() *azureEvaluator {
	return &azureEvaluator{client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *azureEvaluator) Type() string { return "azure_content_safety" }

func (e *azureEvaluator) Info() TypeInfo {
	return typeInfo("azure_content_safety", "Score text with Azure Content Safety.", bothStages(), []SettingField{
		{Name: "endpoint", Type: "string", Description: "Content Safety endpoint", Required: true},
		{Name: "key_env", Type: "string", Description: "Subscription key or env.VAR_NAME", Required: true},
		{Name: "categories", Type: "string_list", Description: "Categories such as Hate, Violence, Sexual, SelfHarm"},
		{Name: "severity_threshold", Type: "number", Description: "Match when a category reaches this severity", Default: 2},
	})
}

func (e *azureEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config azureConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("azure config: %w", err)
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return Evaluation{}, fmt.Errorf("azure config requires endpoint")
	}
	threshold := config.SeverityThreshold
	if threshold <= 0 {
		threshold = 2
	}
	categories := config.Categories
	if len(categories) == 0 {
		categories = []string{"Hate", "Violence", "Sexual", "SelfHarm"}
	}
	endpoint := strings.TrimRight(config.Endpoint, "/") + "/contentsafety/text:analyze?api-version=2024-09-01"
	raw, err := postJSON(ctx, e.client, endpoint, map[string]any{
		"text":       joinMessageText(input.Messages),
		"categories": categories,
		"outputType": "FourSeverityLevels",
	}, "", map[string]string{"Ocp-Apim-Subscription-Key": resolveSecret(config.KeyEnv)})
	if err != nil {
		return Evaluation{}, err
	}
	var body struct {
		CategoriesAnalysis []struct {
			Category string `json:"category"`
			Severity int    `json:"severity"`
		} `json:"categoriesAnalysis"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Evaluation{}, err
	}
	var reasons []string
	for _, category := range body.CategoriesAnalysis {
		if category.Severity >= threshold {
			reasons = append(reasons, fmt.Sprintf("%s:%d", category.Category, category.Severity))
		}
	}
	return Evaluation{Matched: len(reasons) > 0, Reasons: reasons}, nil
}
