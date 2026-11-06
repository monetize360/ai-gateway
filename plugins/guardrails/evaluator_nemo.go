package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// nemoConfig keeps a provider and model only for configs whose checks ask a model.
// Phrase-only configs leave them empty.
type nemoConfig struct {
	ConfigID string `json:"config_id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// nemoEvaluator calls the deployment's NeMo server. Every NeMo policy uses that one server,
// so the address is not stored on the policy. When the config asks a model, the policy's model
// and the requesting caller's own token are forwarded so NeMo calls the gateway as that key.
type nemoEvaluator struct {
	client         *http.Client
	baseURL        string
	internalSecret string
	folderFor      func(tenantID, configID string) string
}

func newNemoEvaluator(baseURL, internalSecret string, folderFor func(tenantID, configID string) string) *nemoEvaluator {
	return &nemoEvaluator{
		client:         &http.Client{Timeout: 30 * time.Second},
		baseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		internalSecret: internalSecret,
		folderFor:      folderFor,
	}
}

func (e *nemoEvaluator) Type() string { return "nemo" }

func (e *nemoEvaluator) Info() TypeInfo {
	return typeInfo("nemo", "Call the open-source NeMo Guardrails server in check-only mode.", bothStages(), []SettingField{
		{Name: "config_id", Type: "string", Description: "NeMo config", Required: true},
		{Name: "provider", Type: "string", Description: "Provider, only for configs that ask a model"},
		{Name: "model", Type: "string", Description: "Model, only for configs that ask a model"},
	})
}

func (e *nemoEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config nemoConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("nemo config: %w", err)
	}
	if e.baseURL == "" {
		return Evaluation{}, fmt.Errorf("NEMO_BASE_URL is not set on the gateway")
	}
	if strings.TrimSpace(config.ConfigID) == "" {
		return Evaluation{}, fmt.Errorf("nemo config requires config_id")
	}
	configID := strings.TrimSpace(config.ConfigID)
	if e.folderFor != nil {
		configID = e.folderFor(policy.TenantID, configID)
	}
	rails := map[string]bool{"input": false, "output": false, "dialog": false, "retrieval": false}
	switch input.Stage {
	case "output":
		rails["output"] = true
	default:
		rails["input"] = true
	}
	messages := make([]map[string]string, 0, len(input.Messages))
	for _, message := range input.Messages {
		role := message.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]string{"role": role, "content": message.Text})
	}
	body := map[string]any{
		"config_id": configID,
		"messages":  messages,
		"options": map[string]any{
			"rails": rails,
			"log":   map[string]any{"activated_rails": true},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Evaluation{}, err
	}
	endpoint := e.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Evaluation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-m360-guardrail-internal", e.internalSecret)
	if model := gatewayModelName(config.Provider, config.Model); model != "" {
		token := callerToken(ctx)
		if token == "" {
			return Evaluation{}, fmt.Errorf("nemo calls a model as the requesting virtual key, but this request has no bearer token")
		}
		req.Header.Set("x-m360-llm-api-key", token)
		req.Header.Set("x-m360-llm-model", model)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return Evaluation{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Evaluation{}, err
	}
	if resp.StatusCode >= 400 {
		return Evaluation{}, fmt.Errorf("nemo status %d", resp.StatusCode)
	}
	return Evaluation{Matched: nemoBlocked(raw), Reasons: []string{config.ConfigID}}, nil
}

type nemoRail struct {
	Stop      *bool    `json:"stop"`
	Decisions []string `json:"decisions"`
}

func nemoBlocked(raw []byte) bool {
	var body struct {
		Log struct {
			ActivatedRails []nemoRail `json:"activated_rails"`
		} `json:"log"`
		Guardrails struct {
			Log struct {
				ActivatedRails []nemoRail `json:"activated_rails"`
			} `json:"log"`
		} `json:"guardrails"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false
	}
	rails := body.Log.ActivatedRails
	if len(rails) == 0 {
		rails = body.Guardrails.Log.ActivatedRails
	}
	for _, rail := range rails {
		if rail.Stop != nil && *rail.Stop {
			return true
		}
		for _, decision := range rail.Decisions {
			if strings.EqualFold(decision, "stop") || strings.EqualFold(decision, "block") {
				return true
			}
		}
	}
	return false
}

// gatewayModelName returns the provider/model id the gateway routes on.
func gatewayModelName(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if model == "" || provider == "" || strings.Contains(model, "/") {
		return model
	}
	return provider + "/" + model
}

func resolveSecret(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "env.") {
		return schemas.NewEnvVar(ref).GetValue()
	}
	if value, ok := os.LookupEnv(ref); ok {
		return value
	}
	return ref
}
