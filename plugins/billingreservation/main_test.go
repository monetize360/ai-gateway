package billingreservation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

func TestPluginReservesWithRequestIdempotency(t *testing.T) {
	t.Parallel()

	var reserve reserveRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = w.Write([]byte(`{"reservationId":"reservation-1","status":"IN_PROGRESS"}`))
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
			"tenant-1": {AccountID: "account-1", WalletID: "wallet-1", CurrencyID: "currency-1"},
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
	if reserve.RequestID != "request-1" || reserve.Amount == "" || reserve.Metadata["estimatedOutputTokens"] != float64(40) {
		t.Fatalf("reserve = %+v", reserve)
	}
	if _, _, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatal(err)
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
