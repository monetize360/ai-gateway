package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type presidioConfig struct {
	AnalyzerURL   string   `json:"analyzer_url"`
	AnonymizerURL string   `json:"anonymizer_url"`
	Entities      []string `json:"entities"`
	Language      string   `json:"language"`
}

type presidioEvaluator struct {
	client *http.Client
}

func newPresidioEvaluator() *presidioEvaluator {
	return &presidioEvaluator{client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *presidioEvaluator) Type() string { return "presidio" }

func (e *presidioEvaluator) Info() TypeInfo {
	return typeInfo("presidio", "Detect personal data with a Presidio analyzer service.", bothStages(), []SettingField{
		{Name: "analyzer_url", Type: "string", Description: "Presidio analyzer base URL", Required: true},
		{Name: "anonymizer_url", Type: "string", Description: "Optional anonymizer base URL used when the action is redact"},
		{Name: "entities", Type: "string_list", Description: "Entity types such as EMAIL_ADDRESS or PHONE_NUMBER"},
		{Name: "language", Type: "string", Description: "Language code", Default: "en"},
	})
}

func (e *presidioEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config presidioConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("presidio config: %w", err)
	}
	if strings.TrimSpace(config.AnalyzerURL) == "" {
		return Evaluation{}, fmt.Errorf("presidio config requires analyzer_url")
	}
	language := strings.TrimSpace(config.Language)
	if language == "" {
		language = "en"
	}
	text := joinMessageText(input.Messages)
	body := map[string]any{"text": text, "language": language}
	if len(config.Entities) > 0 {
		body["entities"] = config.Entities
	}
	raw, err := postJSON(ctx, e.client, strings.TrimRight(config.AnalyzerURL, "/")+"/analyze", body, "", nil)
	if err != nil {
		return Evaluation{}, err
	}
	var findings []struct {
		EntityType string `json:"entity_type"`
	}
	if err := json.Unmarshal(raw, &findings); err != nil {
		return Evaluation{}, err
	}
	if len(findings) == 0 {
		return Evaluation{}, nil
	}
	reasons := make([]string, 0, len(findings))
	for _, finding := range findings {
		reasons = append(reasons, finding.EntityType)
	}
	replacement := ""
	if strings.TrimSpace(config.AnonymizerURL) != "" {
		anonymized, err := postJSON(ctx, e.client, strings.TrimRight(config.AnonymizerURL, "/")+"/anonymize", map[string]any{
			"text":             text,
			"anonymizers":      map[string]any{"DEFAULT": map[string]string{"type": "replace", "new_value": "[REDACTED]"}},
			"analyzer_results": json.RawMessage(raw),
		}, "", nil)
		if err == nil {
			var anon struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(anonymized, &anon) == nil {
				replacement = anon.Text
			}
		}
	}
	return Evaluation{Matched: true, Reasons: reasons, Replacement: replacement}, nil
}
