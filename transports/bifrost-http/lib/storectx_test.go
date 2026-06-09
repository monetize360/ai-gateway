package lib

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestContextWithTimeoutPreservingTenant_FromFastHTTPRequest(t *testing.T) {
	reqCtx := &fasthttp.RequestCtx{}
	reqCtx.SetUserValue(schemas.BifrostContextKeyTenantID, "tenant-2")

	ctx, cancel := ContextWithTimeoutPreservingTenant(reqCtx, time.Second)
	defer cancel()

	if got := TenantIDFromContext(ctx); got != "tenant-2" {
		t.Fatalf("expected tenant-2, got %q", got)
	}
}

func TestContextWithTimeoutPreservingTenant_FromStandardContext(t *testing.T) {
	parent := context.WithValue(context.Background(), schemas.BifrostContextKeyTenantID, "tenant-3")

	ctx, cancel := ContextWithTimeoutPreservingTenant(parent, time.Second)
	defer cancel()

	if got := TenantIDFromContext(ctx); got != "tenant-3" {
		t.Fatalf("expected tenant-3, got %q", got)
	}
}

func TestContextWithTimeoutPreservingTenant_NoTenantUsesParent(t *testing.T) {
	parent := context.WithValue(context.Background(), "other-key", "value")

	ctx, cancel := ContextWithTimeoutPreservingTenant(parent, time.Second)
	defer cancel()

	if got := TenantIDFromContext(ctx); got != "" {
		t.Fatalf("expected empty tenant, got %q", got)
	}
	if got := ctx.Value("other-key"); got != "value" {
		t.Fatalf("expected parent value to be preserved, got %#v", got)
	}
}
