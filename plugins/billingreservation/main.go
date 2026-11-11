// Package billingreservation reserves MPilot wallet funds before an LLM provider call.
package billingreservation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

const (
	PluginName = "billing-reservation"

	reservationContextKey schemas.BifrostContextKey = "mpilot-billing-reservation"

	// defaultInputTokenAllowance is used when default_input_tokens is unset and the
	// prompt is not tokenized. The hold prices that allowance at the model's output
	// rate so a large prompt cannot pass a reservation sized only for completion tokens.
	defaultInputTokenAllowance = 10_000
	// defaultMaxOutputTokenAllowance is used when the request, model estimate, and
	// default_max_output_tokens are all unset.
	defaultMaxOutputTokenAllowance = 10_000
	// defaultHoldTTLSeconds keeps an ACTIVE hold through a long provider call and the
	// rating capture that follows. MPilot expires every ACTIVE hold, including one whose
	// provider call is still in flight.
	defaultHoldTTLSeconds = 3600
	// maxHoldTTLSeconds matches WalletHoldService's one-day cap.
	maxHoldTTLSeconds = 86400

	defaultReservePath = "/api/v1/wallet-holds"
)

// Config configures the MPilot wallet-hold API.
type Config struct {
	BaseURL                string                   `json:"base_url"`
	ReservePath            string                   `json:"reserve_path"`
	TimeoutMS              int                      `json:"timeout_ms"`
	FailClosed             *bool                    `json:"fail_closed"`
	HoldTTLSeconds         int                      `json:"hold_ttl_seconds"`
	DefaultInputTokens     int                      `json:"default_input_tokens"`
	DefaultMaxOutputTokens int                      `json:"default_max_output_tokens"`
	ModelEstimates         map[string]ModelEstimate `json:"model_estimates"`
	TenantFunding          map[string]TenantFunding `json:"tenant_funding"`
	Authorization          *schemas.EnvVar          `json:"authorization,omitempty"`
	AdditionalHeaders      map[string]string        `json:"additional_headers,omitempty"`
}

// TenantFunding maps a tenant to the prepaid wallet rating will charge.
// wallet_id must be that wallet: the service wallet when one exists, otherwise
// the account wallet with no service. A different wallet is ignored at capture
// and the usage is direct-debited.
type TenantFunding struct {
	AccountID      string `json:"account_id"`
	WalletID       string `json:"wallet_id"`
	CurrencyID     string `json:"currency_id,omitempty"`
	PricingVersion string `json:"pricing_version,omitempty"`
}

// ModelEstimate overrides catalog pricing for a provider/model key
// (for example, "openai/gpt-4o-mini") and supplies a safe output bound.
type ModelEstimate struct {
	InputCostPerToken  *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken *float64 `json:"output_cost_per_token,omitempty"`
	MaxOutputTokens    int      `json:"max_output_tokens,omitempty"`
}

type estimate struct {
	inputTokens        int
	outputTokens       int
	inputCostPerToken  float64
	outputCostPerToken float64
}

type reservation struct {
	ID        string
	RequestID string
	Amount    string
	Resource  string
	Released  bool
}

type reserveRequest struct {
	AccountID    string `json:"accountId"`
	WalletID     string `json:"walletId"`
	RequestID    string `json:"requestId"`
	Amount       string `json:"amount"`
	TTLSeconds   int    `json:"ttlSeconds"`
	AllowOverage bool   `json:"allowOverage"`
}

// Plugin implements the LLM hook surface. Reservation happens in PreLLMHook,
// which is the closest public hook to provider execution after routing/cache
// plugin decisions have completed.
type Plugin struct {
	config  Config
	baseURL *url.URL
	client  *http.Client
	catalog *modelcatalog.ModelCatalog
	logger  schemas.Logger
}

func Init(config *Config, catalog *modelcatalog.ModelCatalog, logger schemas.Logger) (*Plugin, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	normalized := *config
	normalized.BaseURL = strings.TrimRight(strings.TrimSpace(normalized.BaseURL), "/")
	if normalized.BaseURL == "" {
		return nil, fmt.Errorf("base_url is required")
	}
	baseURL, err := url.Parse(normalized.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("base_url must be an absolute URL")
	}
	if err := validateRelativePath(&normalized.ReservePath, defaultReservePath); err != nil {
		return nil, fmt.Errorf("reserve_path: %w", err)
	}
	if normalized.TimeoutMS <= 0 {
		normalized.TimeoutMS = 500
	}
	if normalized.TimeoutMS > 5000 {
		return nil, fmt.Errorf("timeout_ms must not exceed 5000")
	}
	if normalized.HoldTTLSeconds <= 0 {
		normalized.HoldTTLSeconds = defaultHoldTTLSeconds
	}
	if normalized.HoldTTLSeconds > maxHoldTTLSeconds {
		return nil, fmt.Errorf("hold_ttl_seconds must not exceed %d", maxHoldTTLSeconds)
	}
	if normalized.DefaultMaxOutputTokens <= 0 {
		normalized.DefaultMaxOutputTokens = defaultMaxOutputTokenAllowance
	}
	if normalized.ModelEstimates == nil {
		normalized.ModelEstimates = make(map[string]ModelEstimate)
	}
	if normalized.TenantFunding == nil {
		normalized.TenantFunding = make(map[string]TenantFunding)
	}
	if normalized.AdditionalHeaders == nil {
		normalized.AdditionalHeaders = make(map[string]string)
	}

	return &Plugin{
		config:  normalized,
		baseURL: baseURL,
		client: &http.Client{
			Timeout: time.Duration(normalized.TimeoutMS) * time.Millisecond,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   2 * time.Second,
				ResponseHeaderTimeout: time.Duration(normalized.TimeoutMS) * time.Millisecond,
			},
		},
		catalog: catalog,
		logger:  logger,
	}, nil
}

func validateRelativePath(path *string, defaultPath string) error {
	if strings.TrimSpace(*path) == "" {
		*path = defaultPath
	}
	parsed, err := url.Parse(*path)
	if err != nil || !strings.HasPrefix(*path, "/") || parsed.IsAbs() || parsed.Host != "" {
		return fmt.Errorf("must be a relative absolute path")
	}
	return nil
}

func (p *Plugin) GetName() string { return PluginName }

func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if ctx == nil || req == nil {
		return req, nil, nil
	}
	provider, model, _ := req.GetRequestFields()
	if provider == "" || model == "" {
		return req, nil, nil // metadata operations do not reserve a model balance.
	}
	requestID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyRequestID)
	tenantID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyTenantID)
	if requestID == "" || tenantID == "" {
		return p.rejectOrSkip(req, "request_id and tenant_id are required for balance reservation")
	}
	estimated, err := p.estimate(req, provider, model)
	if err != nil {
		return p.rejectOrSkip(req, err.Error())
	}
	resource := string(provider) + "/" + model
	amount := estimated.amount()
	if existing, _ := ctx.Value(reservationContextKey).(*reservation); existing != nil && !existing.Released {
		// Fallbacks re-run PreLLMHook on the same context and request ID. Reuse the hold
		// when the new estimate fits. A costlier model releases that hold and reserves the
		// full new amount: wallet holds have no increase operation, and a RELEASED row does
		// not block a new hold for the same request ID.
		fits, cmpErr := amountLessOrEqual(amount, existing.Amount)
		if cmpErr != nil {
			return p.rejectOrSkip(req, cmpErr.Error())
		}
		if fits {
			existing.Resource = resource
			return req, nil, nil
		}
		funding, ok := p.funding(tenantID)
		if !ok {
			return p.rejectOrSkip(req, "tenant funding configuration is unavailable for balance reservation")
		}
		status, replaceErr := p.replaceHold(ctx, existing, funding, requestID, amount, resource)
		if replaceErr != nil {
			if p.failClosed() {
				// Allow cheaper remaining fallbacks to try. A failed release leaves the
				// original hold in place; a failed re-reserve clears it.
				return req, &schemas.LLMPluginShortCircuit{Error: reservationErrorAllowFallbacks(status, replaceErr.Error())}, nil
			}
			p.logger.Warn("billing reservation replace skipped: %v", replaceErr)
			return req, nil, nil
		}
		return req, nil, nil
	}
	funding, ok := p.funding(tenantID)
	if !ok {
		return p.rejectOrSkip(req, "tenant funding configuration is unavailable for balance reservation")
	}
	// One reserve creates an ACTIVE hold. Rating captures it when InferenceUsage
	// carries this same request ID. There is no follow-up start or settle call.
	holdID, status, err := p.reserve(ctx, p.reservePayload(funding, requestID, amount))
	if err != nil {
		if p.failClosed() {
			return req, &schemas.LLMPluginShortCircuit{Error: reservationError(status, err.Error())}, nil
		}
		p.logger.Warn("billing reservation skipped: %v", err)
		return req, nil, nil
	}
	// This custom key lets other gateway plugins inspect the opaque hold
	// without changing Bifrost's request ID. Governance continues to publish that
	// unchanged ID in InferenceUsage, which MPilot Rating uses for capture.
	ctx.SetValue(reservationContextKey, &reservation{
		ID: holdID, RequestID: requestID, Amount: amount, Resource: resource,
	})
	return req, nil, nil
}

// PostLLMHook does not release a successful provider call. Rating captures the
// ACTIVE hold from the InferenceUsage event, and this hook runs before governance
// publishes that event. A terminal failure with no token usage is abandoned, so
// the hold is released instead of waiting for expiry.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if ctx == nil || bifrostErr == nil || responseHasTokenUsage(result) || attemptWillBeRetried(ctx, bifrostErr) {
		return result, bifrostErr, nil
	}
	existing, _ := ctx.Value(reservationContextKey).(*reservation)
	if existing == nil || existing.Released || existing.ID == "" {
		return result, bifrostErr, nil
	}
	if _, err := p.release(ctx, existing.ID, existing.RequestID); err != nil {
		p.logger.Warn("billing reservation release skipped: %v", err)
		return result, bifrostErr, nil
	}
	existing.Released = true
	ctx.SetValue(reservationContextKey, nil)
	return result, bifrostErr, nil
}

func (p *Plugin) Cleanup() error {
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}

func (p *Plugin) funding(tenantID string) (TenantFunding, bool) {
	funding, ok := p.config.TenantFunding[tenantID]
	if !ok || strings.TrimSpace(funding.AccountID) == "" || strings.TrimSpace(funding.WalletID) == "" {
		return TenantFunding{}, false
	}
	return funding, true
}

func (p *Plugin) reservePayload(funding TenantFunding, requestID, amount string) reserveRequest {
	return reserveRequest{
		AccountID:    funding.AccountID,
		WalletID:     funding.WalletID,
		RequestID:    requestID,
		Amount:       amount,
		TTLSeconds:   p.config.HoldTTLSeconds,
		AllowOverage: false,
	}
}

func (p *Plugin) failClosed() bool {
	return p.config.FailClosed == nil || *p.config.FailClosed
}

func (p *Plugin) rejectOrSkip(req *schemas.BifrostRequest, message string) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if !p.failClosed() {
		p.logger.Warn("billing reservation skipped: %s", message)
		return req, nil, nil
	}
	return req, &schemas.LLMPluginShortCircuit{Error: reservationError(http.StatusServiceUnavailable, message)}, nil
}

func reservationError(status int, message string) *schemas.BifrostError {
	return reservationErrorWithFallback(status, message, false)
}

func reservationErrorAllowFallbacks(status int, message string) *schemas.BifrostError {
	return reservationErrorWithFallback(status, message, true)
}

func reservationErrorWithFallback(status int, message string, allowFallbacks bool) *schemas.BifrostError {
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	return &schemas.BifrostError{
		Type:           bifrost.Ptr("billing_reservation_failed"),
		StatusCode:     bifrost.Ptr(status),
		Error:          &schemas.ErrorField{Message: message},
		AllowFallbacks: bifrost.Ptr(allowFallbacks),
	}
}

func amountLessOrEqual(left, right string) (bool, error) {
	leftAmount, okLeft := new(big.Rat).SetString(strings.TrimSpace(left))
	rightAmount, okRight := new(big.Rat).SetString(strings.TrimSpace(right))
	if !okLeft || !okRight {
		return false, fmt.Errorf("authorized amount comparison failed")
	}
	return leftAmount.Cmp(rightAmount) <= 0, nil
}

func amountDelta(target, current string) (string, error) {
	targetAmount, okTarget := new(big.Rat).SetString(strings.TrimSpace(target))
	currentAmount, okCurrent := new(big.Rat).SetString(strings.TrimSpace(current))
	if !okTarget || !okCurrent {
		return "", fmt.Errorf("authorized amount comparison failed")
	}
	delta := new(big.Rat).Sub(targetAmount, currentAmount)
	if delta.Sign() <= 0 {
		return "", fmt.Errorf("increase requires a positive additional amount")
	}
	return delta.FloatString(8), nil
}

// attemptWillBeRetried reports whether Bifrost will dispatch another fallback for
// this failed attempt. PostLLMHook must not release the hold in that case.
// A cancellation is terminal even when fallbacks remain.
func attemptWillBeRetried(ctx context.Context, err *schemas.BifrostError) bool {
	if err == nil {
		return false
	}
	if err.Error != nil && err.Error.Type != nil && *err.Error.Type == schemas.RequestCancelled {
		return false
	}
	if err.AllowFallbacks != nil && !*err.AllowFallbacks {
		return false
	}
	fallbackCount := bifrost.GetIntFromContext(ctx, schemas.BifrostContextKeyFallbackCount)
	if fallbackCount <= 0 {
		return false
	}
	fallbackIndex := bifrost.GetIntFromContext(ctx, schemas.BifrostContextKeyFallbackIndex)
	return fallbackIndex < fallbackCount
}

func responseHasTokenUsage(result *schemas.BifrostResponse) bool {
	prompt, completion := tokenCounts(result)
	return prompt > 0 || completion > 0
}

func tokenCounts(result *schemas.BifrostResponse) (promptTokens, completionTokens int) {
	if result == nil {
		return 0, 0
	}
	switch {
	case result.TextCompletionResponse != nil && result.TextCompletionResponse.Usage != nil:
		return result.TextCompletionResponse.Usage.PromptTokens, result.TextCompletionResponse.Usage.CompletionTokens
	case result.ChatResponse != nil && result.ChatResponse.Usage != nil:
		return result.ChatResponse.Usage.PromptTokens, result.ChatResponse.Usage.CompletionTokens
	case result.ResponsesResponse != nil && result.ResponsesResponse.Usage != nil:
		return result.ResponsesResponse.Usage.InputTokens, result.ResponsesResponse.Usage.OutputTokens
	case result.ResponsesStreamResponse != nil && result.ResponsesStreamResponse.Response != nil && result.ResponsesStreamResponse.Response.Usage != nil:
		return result.ResponsesStreamResponse.Response.Usage.InputTokens, result.ResponsesStreamResponse.Response.Usage.OutputTokens
	case result.EmbeddingResponse != nil && result.EmbeddingResponse.Usage != nil:
		return result.EmbeddingResponse.Usage.PromptTokens, 0
	default:
		return 0, 0
	}
}

func (p *Plugin) estimate(req *schemas.BifrostRequest, provider schemas.ModelProvider, model string) (estimate, error) {
	entry, ok := p.config.ModelEstimates[string(provider)+"/"+model]
	if !ok {
		entry = p.config.ModelEstimates[model]
	}
	inputRate, outputRate := entry.InputCostPerToken, entry.OutputCostPerToken
	if (inputRate == nil || outputRate == nil) && p.catalog != nil {
		catalogInput, catalogOutput := p.catalog.GetChatTokenRates(string(provider), model)
		if inputRate == nil {
			inputRate = catalogInput
		}
		if outputRate == nil {
			outputRate = catalogOutput
		}
	}
	if inputRate == nil || outputRate == nil {
		return estimate{}, fmt.Errorf("pricing is unavailable for %s/%s", provider, model)
	}
	outputTokens := requestedMaxOutputTokens(req)
	if outputTokens <= 0 {
		outputTokens = entry.MaxOutputTokens
	}
	if outputTokens <= 0 {
		outputTokens = p.config.DefaultMaxOutputTokens
	}
	if outputTokens <= 0 {
		return estimate{}, fmt.Errorf("max output tokens are unavailable for %s/%s", provider, model)
	}
	if p.config.DefaultInputTokens < 0 || outputTokens < 0 {
		return estimate{}, fmt.Errorf("token estimates cannot be negative")
	}
	inputTokens := p.config.DefaultInputTokens
	inputCostPerToken := *inputRate
	if inputTokens == 0 {
		// Prompt inspection is unavailable here; reserve a conservative input
		// floor of output_cost_per_token * 10_000 so settlement cannot exceed a
		// hold that ignored prompt size.
		inputTokens = defaultInputTokenAllowance
		inputCostPerToken = *outputRate
	}
	return estimate{inputTokens: inputTokens, outputTokens: outputTokens, inputCostPerToken: inputCostPerToken, outputCostPerToken: *outputRate}, nil
}

func requestedMaxOutputTokens(req *schemas.BifrostRequest) int {
	switch {
	case req.ChatRequest != nil && req.ChatRequest.Params != nil && req.ChatRequest.Params.MaxCompletionTokens != nil:
		return *req.ChatRequest.Params.MaxCompletionTokens
	case req.TextCompletionRequest != nil && req.TextCompletionRequest.Params != nil && req.TextCompletionRequest.Params.MaxTokens != nil:
		return *req.TextCompletionRequest.Params.MaxTokens
	case req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil && req.ResponsesRequest.Params.MaxOutputTokens != nil:
		return *req.ResponsesRequest.Params.MaxOutputTokens
	default:
		return 0
	}
}

func (e estimate) amount() string {
	// MPilot accepts a decimal string as BigDecimal. Never send the binary floating-point
	// representation used by the model catalog as the financial authorization amount.
	return strconv.FormatFloat(float64(e.inputTokens)*e.inputCostPerToken+
		float64(e.outputTokens)*e.outputCostPerToken, 'f', 8, 64)
}

func (p *Plugin) reserve(ctx context.Context, payload reserveRequest) (string, int, error) {
	body, status, err := p.post(ctx, p.config.ReservePath, payload.RequestID, payload.RequestID, payload)
	if err != nil {
		return "", status, err
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &response); err != nil || strings.TrimSpace(response.ID) == "" {
		return "", status, fmt.Errorf("reserve response did not include id")
	}
	return response.ID, status, nil
}

// replaceHold releases the current ACTIVE hold and reserves the full new amount
// on the same request ID. The in-memory hold is cleared once release succeeds, so
// a failed re-reserve cannot be reused by a later fallback.
func (p *Plugin) replaceHold(ctx *schemas.BifrostContext, existing *reservation, funding TenantFunding, requestID, amount, resource string) (int, error) {
	status, err := p.release(ctx, existing.ID, requestID)
	if err != nil {
		return status, err
	}
	ctx.SetValue(reservationContextKey, nil)
	holdID, status, err := p.reserve(ctx, p.reservePayload(funding, requestID, amount))
	if err != nil {
		return status, err
	}
	ctx.SetValue(reservationContextKey, &reservation{
		ID: holdID, RequestID: requestID, Amount: amount, Resource: resource,
	})
	return status, nil
}

func (p *Plugin) release(ctx context.Context, holdID, requestID string) (int, error) {
	path := strings.TrimRight(p.config.ReservePath, "/") + "/" + url.PathEscape(holdID) + "/release"
	_, status, err := p.post(ctx, path, requestID, requestID+":release:"+holdID, struct{}{})
	return status, err
}

func (p *Plugin) post(parent context.Context, path, requestID, idempotencyKey string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}
	requestURL := p.baseURL.ResolveReference(&url.URL{Path: path})
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.config.TimeoutMS)*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	// Preserve the exact Bifrost request ID so MPilot can correlate the hold
	// with the existing Kafka InferenceUsage event.
	request.Header.Set("X-Request-Id", requestID)
	for key, value := range p.config.AdditionalHeaders {
		request.Header.Set(key, value)
	}
	if p.config.Authorization != nil && strings.TrimSpace(p.config.Authorization.GetValue()) != "" {
		request.Header.Set("Authorization", p.config.Authorization.GetValue())
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("request wallet hold API: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read wallet hold API response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, fmt.Errorf("wallet hold API returned %d: %s", response.StatusCode, safeResponseMessage(responseBody))
	}
	return responseBody, response.StatusCode, nil
}

func safeResponseMessage(body []byte) string {
	message := strings.TrimSpace(string(body))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

var _ schemas.LLMPlugin = (*Plugin)(nil)
