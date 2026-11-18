package guardrails

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type bedrockConfig struct {
	GuardrailID      string `json:"guardrail_id"`
	GuardrailVersion string `json:"guardrail_version"`
	Region           string `json:"region"`
	AccessKeyEnv     string `json:"access_key_env"`
	SecretKeyEnv     string `json:"secret_key_env"`
	SessionTokenEnv  string `json:"session_token_env"`
}

type bedrockEvaluator struct {
	client *http.Client
}

func newBedrockEvaluator() *bedrockEvaluator {
	return &bedrockEvaluator{client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *bedrockEvaluator) Type() string { return "bedrock" }

func (e *bedrockEvaluator) Info() TypeInfo {
	return typeInfo("bedrock", "Apply an AWS Bedrock Guardrail to the message text.", bothStages(), []SettingField{
		{Name: "guardrail_id", Type: "string", Description: "Bedrock guardrail identifier", Required: true},
		{Name: "guardrail_version", Type: "string", Description: "Guardrail version", Required: true},
		{Name: "region", Type: "string", Description: "AWS region", Required: true},
		{Name: "access_key_env", Type: "string", Description: "Access key or env.VAR_NAME", Required: true},
		{Name: "secret_key_env", Type: "string", Description: "Secret key or env.VAR_NAME", Required: true},
		{Name: "session_token_env", Type: "string", Description: "Optional session token or env.VAR_NAME"},
	})
}

func (e *bedrockEvaluator) Evaluate(ctx context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	var config bedrockConfig
	if err := json.Unmarshal([]byte(policy.ConfigJSON), &config); err != nil {
		return Evaluation{}, fmt.Errorf("bedrock config: %w", err)
	}
	if strings.TrimSpace(config.GuardrailID) == "" || strings.TrimSpace(config.GuardrailVersion) == "" || strings.TrimSpace(config.Region) == "" {
		return Evaluation{}, fmt.Errorf("bedrock config requires guardrail_id, guardrail_version, and region")
	}
	source := "INPUT"
	if input.Stage == "output" {
		source = "OUTPUT"
	}
	payload, err := json.Marshal(map[string]any{
		"source": source,
		"content": []map[string]any{
			{"text": map[string]string{"text": joinMessageText(input.Messages)}},
		},
	})
	if err != nil {
		return Evaluation{}, err
	}
	endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/guardrail/%s/version/%s/apply",
		config.Region, url.PathEscape(config.GuardrailID), url.PathEscape(config.GuardrailVersion))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Evaluation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	signAWS(req, payload, config.Region, resolveSecret(config.AccessKeyEnv), resolveSecret(config.SecretKeyEnv), resolveSecret(config.SessionTokenEnv))
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
		return Evaluation{}, fmt.Errorf("bedrock status %d", resp.StatusCode)
	}
	var body struct {
		Action  string `json:"action"`
		Outputs []struct {
			Text string `json:"text"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Evaluation{}, err
	}
	replacement := ""
	if len(body.Outputs) > 0 {
		replacement = body.Outputs[0].Text
	}
	matched := strings.EqualFold(body.Action, "GUARDRAIL_INTERVENED")
	return Evaluation{Matched: matched, Reasons: []string{body.Action}, Replacement: replacement}, nil
}

func signAWS(req *http.Request, payload []byte, region, accessKey, secretKey, sessionToken string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.URL.Host)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}
	signedHeaders := "host;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" + "x-amz-date:" + amzDate + "\n"
	if sessionToken != "" {
		signedHeaders = "host;x-amz-date;x-amz-security-token"
		canonicalHeaders += "x-amz-security-token:" + sessionToken + "\n"
	}
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := dateStamp + "/" + region + "/bedrock/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+secretKey), dateStamp), region), "bedrock"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, signedHeaders, signature))
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}
