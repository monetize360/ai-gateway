package billingreservation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

func TestPluginReservesWithRequestIdempotency(t *testing.T) {
	t.Parallel()

	var calls int
	var reserve reserveRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("X-Request-Id"); got != "request-1" {
			t.Errorf("x-request-id = %q, want request-1", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "request-1" {
			t.Errorf("idempotency key = %q, want request-1", got)
		}
		switch r.URL.Path {
		case "/reserve":
			if err := json.NewDecoder(r.Body).Decode(&reserve); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"id":"hold-1","status":"ACTIVE"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	inputRate, outputRate := 0.001, 0.002
	plugin, err := Init(&Config{
		BaseURL:     server.URL,
		ReservePath: "/reserve",
		ModelEstimates: map[string]ModelEstimate{"openai/test-model": {
			InputCostPerToken:  &inputRate,
			OutputCostPerToken: &outputRate,
			MaxOutputTokens:    40,
		}},
		TenantFunding: map[string]TenantFunding{
			"tenant-1": {AccountID: "account-1", WalletID: "wallet-1"},
		},
	}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyTenantID, "tenant-1")
	ctx.SetValue(schemas.BifrostContextKeyGovernanceOrgID, "org-1")
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "test-model",
	}}
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, req); err != nil || shortCircuit != nil {
		t.Fatalf("PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}
	// Unset default_input_tokens holds output_cost_per_token * 10_000 for input.
	wantAmount := strconv.FormatFloat(10_000*outputRate+40*outputRate, 'f', 8, 64)
	if reserve.AccountID != "account-1" || reserve.WalletID != "wallet-1" ||
		reserve.RequestID != "request-1" || reserve.Amount != wantAmount ||
		reserve.TTLSeconds != defaultHoldTTLSeconds || reserve.AllowOverage {
		t.Fatalf("reserve = %+v, want amount %s ttl %d", reserve, wantAmount, defaultHoldTTLSeconds)
	}
	// A successful completion stays held so rating can capture it.
	if _, _, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 reserve and no release", calls)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestEstimateDefaultsInputHoldToOutputCostTimesAllowance(t *testing.T) {
	t.Parallel()

	inputRate, outputRate := 0.001, 0.002
	plugin, err := Init(&Config{
		BaseURL: "https://mpilot.example",
		ModelEstimates: map[string]ModelEstimate{"openai/test-model": {
			InputCostPerToken:  &inputRate,
			OutputCostPerToken: &outputRate,
			MaxOutputTokens:    10,
		}},
	}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}

	got, err := plugin.estimate(&schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "test-model",
	}}, schemas.OpenAI, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if got.inputTokens != 10_000 || got.inputCostPerToken != outputRate || got.amount() != strconv.FormatFloat(10_000*outputRate+10*outputRate, 'f', 8, 64) {
		t.Fatalf("estimate = %+v amount=%s", got, got.amount())
	}

	plugin.config.DefaultInputTokens = 1_000
	got, err = plugin.estimate(&schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "test-model",
	}}, schemas.OpenAI, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if got.inputTokens != 1_000 || got.inputCostPerToken != inputRate || got.amount() != strconv.FormatFloat(1_000*inputRate+10*outputRate, 'f', 8, 64) {
		t.Fatalf("explicit default_input_tokens estimate = %+v amount=%s", got, got.amount())
	}

	plugin.config.DefaultInputTokens = 0
	plugin.config.DefaultMaxOutputTokens = 10_000
	got, err = plugin.estimate(&schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "test-model",
	}}, schemas.OpenAI, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	// Model estimate MaxOutputTokens=10 still wins over the 10k default.
	if got.outputTokens != 10 {
		t.Fatalf("outputTokens = %d, want model estimate 10", got.outputTokens)
	}

	plugin.config.ModelEstimates = map[string]ModelEstimate{"openai/test-model": {
		InputCostPerToken:  &inputRate,
		OutputCostPerToken: &outputRate,
	}}
	got, err = plugin.estimate(&schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "test-model",
	}}, schemas.OpenAI, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if got.inputTokens != 10_000 || got.outputTokens != 10_000 ||
		got.amount() != strconv.FormatFloat(10_000*outputRate+10_000*outputRate, 'f', 8, 64) {
		t.Fatalf("default output estimate = %+v amount=%s", got, got.amount())
	}
}

func TestPluginRejectsWhenPricingIsUnavailable(t *testing.T) {
	t.Parallel()

	plugin, err := Init(&Config{BaseURL: "https://mpilot.example"}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyTenantID, "tenant-1")
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "unknown"}}
	_, shortCircuit, hookErr := plugin.PreLLMHook(ctx, req)
	if hookErr != nil || shortCircuit == nil || shortCircuit.Error == nil {
		t.Fatalf("PreLLMHook() = shortCircuit %v, err %v; want rejection", shortCircuit, hookErr)
	}
	if got := *shortCircuit.Error.StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestPluginBlocksProviderWhenReserveFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "insufficient available balance", http.StatusConflict)
	}))
	defer server.Close()
	rate := 0.001
	plugin, err := Init(&Config{BaseURL: server.URL, ReservePath: "/reserve", ModelEstimates: map[string]ModelEstimate{
		"openai/test-model": {InputCostPerToken: &rate, OutputCostPerToken: &rate, MaxOutputTokens: 1},
	}, TenantFunding: map[string]TenantFunding{
		"tenant-1": {AccountID: "account-1", WalletID: "wallet-1", CurrencyID: "currency-1"},
	}}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyTenantID, "tenant-1")
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "test-model"}}
	_, shortCircuit, hookErr := plugin.PreLLMHook(ctx, req)
	if hookErr != nil || shortCircuit == nil || shortCircuit.Error == nil {
		t.Fatalf("PreLLMHook() = shortCircuit %v, err %v; want reserve rejection", shortCircuit, hookErr)
	}
}

func TestPluginReplacesHoldWhenFallbackEstimateIsHigher(t *testing.T) {
	t.Parallel()

	var reserves []reserveRequest
	var releasePath, releaseRequestID string
	cheapRate, expensiveRate := 0.001, 0.01
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/reserve" && r.Method == http.MethodPost:
			var body reserveRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			reserves = append(reserves, body)
			_, _ = w.Write([]byte(`{"id":"hold-` + strconv.Itoa(len(reserves)) + `","status":"ACTIVE"}`))
		case r.URL.Path == "/reserve/hold-1/release" && r.Method == http.MethodPost:
			releasePath = r.URL.Path
			releaseRequestID = r.Header.Get("X-Request-Id")
			_, _ = w.Write([]byte(`{"id":"hold-1","status":"RELEASED"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	plugin, err := Init(&Config{
		BaseURL:     server.URL,
		ReservePath: "/reserve",
		ModelEstimates: map[string]ModelEstimate{
			"openai/cheap":     {InputCostPerToken: &cheapRate, OutputCostPerToken: &cheapRate, MaxOutputTokens: 1},
			"openai/expensive": {InputCostPerToken: &expensiveRate, OutputCostPerToken: &expensiveRate, MaxOutputTokens: 1},
		},
		TenantFunding: map[string]TenantFunding{
			"tenant-1": {AccountID: "account-1", WalletID: "wallet-1"},
		},
	}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyTenantID, "tenant-1")
	cheapReq := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "cheap"}}
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, cheapReq); err != nil || shortCircuit != nil {
		t.Fatalf("primary PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}

	// Same context, costlier fallback model — release the cheap hold and reserve the full new amount.
	ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 1)
	expensiveReq := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "expensive"}}
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, expensiveReq); err != nil || shortCircuit != nil {
		t.Fatalf("fallback PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}

	if len(reserves) != 2 || releasePath != "/reserve/hold-1/release" {
		t.Fatalf("reserves=%d release=%q, want 2 reserves and /reserve/hold-1/release", len(reserves), releasePath)
	}
	wantAmount := strconv.FormatFloat(10_000*expensiveRate+1*expensiveRate, 'f', 8, 64)
	if reserves[1].Amount != wantAmount || reserves[1].RequestID != "request-1" || reserves[1].AllowOverage {
		t.Fatalf("replacement reserve = %+v, want amount %s", reserves[1], wantAmount)
	}
	if releaseRequestID != "request-1" {
		t.Fatalf("x-request-id = %q, want request-1", releaseRequestID)
	}
	held, _ := ctx.Value(reservationContextKey).(*reservation)
	if held == nil || held.ID != "hold-2" || held.Amount != wantAmount {
		t.Fatalf("held = %+v, want hold-2 at %s", held, wantAmount)
	}

	// The same estimate on a later attempt reuses the replacement hold.
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, expensiveReq); err != nil || shortCircuit != nil {
		t.Fatalf("repeat fallback PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}
	if len(reserves) != 2 {
		t.Fatalf("reserves after reuse = %d, want 2", len(reserves))
	}
}

func TestPluginReleasesTerminalFailureWithoutUsage(t *testing.T) {
	t.Parallel()

	var releaseCalls int
	var releasePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reserve":
			_, _ = w.Write([]byte(`{"id":"hold-1","status":"ACTIVE"}`))
		case "/reserve/hold-1/release":
			releaseCalls++
			releasePath = r.URL.Path
			if got := r.Header.Get("Idempotency-Key"); got != "request-1:release:hold-1" {
				t.Errorf("idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"id":"hold-1","status":"RELEASED"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	rate := 0.001
	plugin := testPlugin(t, server.URL, rate)
	ctx := testContext()
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "test-model"}}
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, req); err != nil || shortCircuit != nil {
		t.Fatalf("PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}

	providerErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "provider down"}}
	if _, _, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, providerErr); err != nil {
		t.Fatal(err)
	}
	if releaseCalls != 1 || releasePath != "/reserve/hold-1/release" {
		t.Fatalf("releaseCalls=%d path=%q", releaseCalls, releasePath)
	}
	if _, _, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, providerErr); err != nil {
		t.Fatal(err)
	}
	if releaseCalls != 1 {
		t.Fatalf("releaseCalls after repeat = %d, want 1", releaseCalls)
	}
}

func TestPluginKeepsHoldWhenFailureHasUsageOrWillRetry(t *testing.T) {
	t.Parallel()

	var releaseCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reserve":
			_, _ = w.Write([]byte(`{"id":"hold-1","status":"ACTIVE"}`))
		default:
			releaseCalls++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	rate := 0.001
	plugin := testPlugin(t, server.URL, rate)
	ctx := testContext()
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "test-model"}}
	if _, shortCircuit, err := plugin.PreLLMHook(ctx, req); err != nil || shortCircuit != nil {
		t.Fatalf("PreLLMHook() = shortCircuit %v, err %v", shortCircuit, err)
	}

	used := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 3, CompletionTokens: 1},
	}}
	providerErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "provider down"}}
	if _, _, err := plugin.PostLLMHook(ctx, used, providerErr); err != nil {
		t.Fatal(err)
	}

	ctx.SetValue(schemas.BifrostContextKeyFallbackCount, 2)
	ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 0)
	if _, _, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, providerErr); err != nil {
		t.Fatal(err)
	}
	if releaseCalls != 0 {
		t.Fatalf("releaseCalls = %d, want 0", releaseCalls)
	}
}

func TestHoldTTLDefaultsAndCap(t *testing.T) {
	t.Parallel()

	plugin, err := Init(&Config{BaseURL: "https://mpilot.example"}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	if plugin.config.ReservePath != defaultReservePath || plugin.config.HoldTTLSeconds != defaultHoldTTLSeconds {
		t.Fatalf("path=%s ttl=%d", plugin.config.ReservePath, plugin.config.HoldTTLSeconds)
	}
	if _, err := Init(&Config{BaseURL: "https://mpilot.example", HoldTTLSeconds: maxHoldTTLSeconds + 1}, nil, bifrost.NewNoOpLogger()); err == nil {
		t.Fatal("expected hold_ttl_seconds above one day to be rejected")
	}
}

func testPlugin(t *testing.T, baseURL string, rate float64) *Plugin {
	t.Helper()
	plugin, err := Init(&Config{
		BaseURL:     baseURL,
		ReservePath: "/reserve",
		ModelEstimates: map[string]ModelEstimate{
			"openai/test-model": {InputCostPerToken: &rate, OutputCostPerToken: &rate, MaxOutputTokens: 1},
		},
		TenantFunding: map[string]TenantFunding{
			"tenant-1": {AccountID: "account-1", WalletID: "wallet-1"},
		},
	}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

func testContext() *schemas.BifrostContext {
	ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyTenantID, "tenant-1")
	return ctx
}

func TestAmountDelta(t *testing.T) {
	t.Parallel()
	got, err := amountDelta("20.50000000", "10.25000000")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.25000000" {
		t.Fatalf("delta = %s, want 10.25000000", got)
	}
	fits, err := amountLessOrEqual("10.25000000", "20.50000000")
	if err != nil || !fits {
		t.Fatalf("amountLessOrEqual = %v, err %v", fits, err)
	}
}
