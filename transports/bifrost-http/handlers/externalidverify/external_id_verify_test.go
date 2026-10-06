package externalidverify_test

import (
	"context"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/handlers"
)

func TestChatRequestParsesExternalIDFromBody(t *testing.T) {
	t.Parallel()

	body := `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"external_id":"  txn-abc  "}`
	var req handlers.ChatRequest
	if err := sonic.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ExternalID != "txn-abc" {
		t.Fatalf("ExternalID = %q, want txn-abc", req.ExternalID)
	}
}

func TestChatRequestIgnoresCamelCaseExternalId(t *testing.T) {
	t.Parallel()

	body := `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"externalId":"should-ignore"}`
	var req handlers.ChatRequest
	if err := sonic.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ExternalID != "" {
		t.Fatalf("ExternalID = %q, want empty (only external_id is supported)", req.ExternalID)
	}
}

func TestChatRequestBlankWhenExternalIDOmitted(t *testing.T) {
	t.Parallel()

	body := `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	var req handlers.ChatRequest
	if err := sonic.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ExternalID != "" {
		t.Fatalf("ExternalID = %q, want empty", req.ExternalID)
	}
}

func TestRequestIDOverrideMatchesInferenceUsageSource(t *testing.T) {
	t.Parallel()

	// Mirrors applyChatExternalID: body external_id becomes Bifrost request-id,
	// which publishInferenceUsage reads as externalTransactionId.
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "generated-uuid")
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "txn-from-body")

	got, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	if got != "txn-from-body" {
		t.Fatalf("request-id = %q, want txn-from-body", got)
	}
}
