//go:build live

package billingreservation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// TestLiveConcurrentReservesDoNotOverdraw drives the billing-reservation plugin
// (the gateway PreLLMHook that calls MPilot) against a locally running MPilot
// API. Twenty requests each try to hold 0.25 on a wallet whose balance is 1.00.
// The wallet guard must accept exactly four holds and leave available balance at 0.
func TestLiveConcurrentReservesDoNotOverdraw(t *testing.T) {
	baseURL := os.Getenv("MPILOT_BASE_URL")
	token := os.Getenv("MPILOT_TOKEN")
	accountID := os.Getenv("MPILOT_ACCOUNT_ID")
	walletID := os.Getenv("MPILOT_WALLET_ID")
	if baseURL == "" || token == "" || accountID == "" || walletID == "" {
		t.Fatal("MPILOT_BASE_URL, MPILOT_TOKEN, MPILOT_ACCOUNT_ID and MPILOT_WALLET_ID are required")
	}

	inputRate, outputRate := 0.125, 0.125
	plugin, err := Init(&Config{
		BaseURL:                baseURL,
		ReservePath:            "/api/v1/wallet-holds",
		DefaultInputTokens:     1,
		DefaultMaxOutputTokens: 1,
		HoldTTLSeconds:         300,
		ModelEstimates: map[string]ModelEstimate{"openai/live-concurrency": {
			InputCostPerToken:  &inputRate,
			OutputCostPerToken: &outputRate,
		}},
		TenantFunding: map[string]TenantFunding{
			"live-tenant": {AccountID: accountID, WalletID: walletID},
		},
		Authorization: schemas.NewEnvVar("Bearer " + token),
	}, nil, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}

	const requests = 20
	const wantHolds = 4
	wantAmount := strconv.FormatFloat(inputRate+outputRate, 'f', 8, 64)
	if wantAmount != "0.25000000" {
		t.Fatalf("estimate amount = %s, want 0.25000000", wantAmount)
	}

	type outcome struct {
		holdID string
		status int
		err    string
		ctx    *schemas.BifrostContext
	}
	results := make([]outcome, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for i := range requests {
		go func(i int) {
			defer wg.Done()
			requestID := fmt.Sprintf("live-conc-%02d", i)
			ctx := schemas.NewBifrostContextWithValue(context.Background(), schemas.NoDeadline, schemas.BifrostContextKeyRequestID, requestID)
			ctx.SetValue(schemas.BifrostContextKeyTenantID, "live-tenant")
			req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "live-concurrency",
			}}
			_, shortCircuit, hookErr := plugin.PreLLMHook(ctx, req)
			switch {
			case hookErr != nil:
				results[i] = outcome{err: hookErr.Error(), ctx: ctx}
			case shortCircuit != nil && shortCircuit.Error != nil:
				status := 0
				if shortCircuit.Error.StatusCode != nil {
					status = *shortCircuit.Error.StatusCode
				}
				results[i] = outcome{status: status, err: shortCircuit.Error.Error.Message, ctx: ctx}
			default:
				held, _ := ctx.Value(reservationContextKey).(*reservation)
				holdID := ""
				if held != nil {
					holdID = held.ID
				}
				results[i] = outcome{holdID: holdID, status: 201, ctx: ctx}
			}
		}(i)
	}
	wg.Wait()

	var held int
	var rejected int
	for i, result := range results {
		switch {
		case result.holdID != "":
			held++
			t.Logf("request %02d reserved hold %s", i, result.holdID)
		case result.status == 409:
			rejected++
			t.Logf("request %02d rejected 409: %s", i, result.err)
		default:
			t.Errorf("request %02d unexpected status=%d err=%s", i, result.status, result.err)
		}
	}
	if held != wantHolds || rejected != requests-wantHolds {
		t.Fatalf("holds=%d rejected=%d, want %d holds and %d rejections", held, rejected, wantHolds, requests-wantHolds)
	}

	balance, reserved, available, active := walletSnapshot(t, walletID)
	t.Logf("after burst balance=%s reserved=%s available=%s active_holds=%s", balance, reserved, available, active)
	if balance != "1.00000000" || reserved != "1.00000000" || available != "0.00000000" || active != strconv.Itoa(wantHolds) {
		t.Fatalf("wallet overcommitted or under-held: balance=%s reserved=%s available=%s active=%s", balance, reserved, available, active)
	}
	if strings.HasPrefix(available, "-") {
		t.Fatal("available balance went negative")
	}

	providerErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "provider down"}}
	for _, result := range results {
		if result.holdID == "" {
			continue
		}
		if _, _, err := plugin.PostLLMHook(result.ctx, &schemas.BifrostResponse{}, providerErr); err != nil {
			t.Fatal(err)
		}
	}
	balance, reserved, available, active = walletSnapshot(t, walletID)
	t.Logf("after release balance=%s reserved=%s available=%s active_holds=%s", balance, reserved, available, active)
	if balance != "1.00000000" || reserved != "0.00000000" || available != "1.00000000" || active != "0" {
		t.Fatalf("release did not restore the wallet: balance=%s reserved=%s available=%s active=%s", balance, reserved, available, active)
	}
}

func walletSnapshot(t *testing.T, walletID string) (balance, reserved, available, active string) {
	t.Helper()
	query := fmt.Sprintf(`SELECT to_char(w.balance, 'FM999999990.00000000'),
		to_char(w.reserved_balance, 'FM999999990.00000000'),
		to_char(w.available_balance, 'FM999999990.00000000'),
		(SELECT COUNT(*) FROM wallet_hold h WHERE h.wallet_id = w.id AND h.status = 'ACTIVE')::text
		FROM wallet__m w WHERE w.id = '%s'`, walletID)
	out, err := exec.Command("psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "tenant2", "-tA", "-c", query).Output()
	if err != nil {
		t.Fatalf("wallet snapshot: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(fields) != 4 {
		t.Fatalf("wallet snapshot = %q", strings.TrimSpace(string(out)))
	}
	return fields[0], fields[1], fields[2], fields[3]
}
