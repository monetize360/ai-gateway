package guardrails

import (
	"context"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

type callerTokenContextKey struct{}

// withCallerToken carries the requesting caller's bearer token on a context that has no request
// headers, such as a background input check or a test call.
func withCallerToken(ctx context.Context, token string) context.Context {
	token = strings.TrimSpace(token)
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, callerTokenContextKey{}, token)
}

// callerToken returns the bearer token the request came in with. A model-based check calls the
// gateway with it, so that call is made, limited, and billed as the same virtual key.
func callerToken(ctx context.Context) string {
	if token, ok := ctx.Value(callerTokenContextKey{}).(string); ok && token != "" {
		return token
	}
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return bearerFromHeaders(headers)
}

func bearerFromHeaders(headers map[string]string) string {
	value := headerValue(headers, "authorization")
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
