package schemas

import (
	"crypto/subtle"
	"strings"
	"sync/atomic"
)

// guardrailInternalCredential is the header name and secret the guardrails plugin attaches to
// the model calls it makes on a policy's behalf (NeMo self-check, LLM judge).
type guardrailInternalCredential struct {
	header string
	secret string
}

var registeredGuardrailInternalCredential atomic.Pointer[guardrailInternalCredential]

// RegisterGuardrailInternalCall records the header and per-process secret that mark a request as a
// guardrail's own model call. The guardrails plugin registers it at startup. Registering an empty
// header or secret turns the marker off.
func RegisterGuardrailInternalCall(header, secret string) {
	header = strings.ToLower(strings.TrimSpace(header))
	if header == "" || secret == "" {
		registeredGuardrailInternalCredential.Store(nil)
		return
	}
	registeredGuardrailInternalCredential.Store(&guardrailInternalCredential{header: header, secret: secret})
}

// IsGuardrailInternalCall reports whether the request carries the registered secret. The secret is
// random per process and never leaves the gateway and its sidecars, so a caller cannot forge it.
func IsGuardrailInternalCall(ctx *BifrostContext) bool {
	credential := registeredGuardrailInternalCredential.Load()
	if credential == nil || ctx == nil {
		return false
	}
	headers, _ := ctx.Value(BifrostContextKeyRequestHeaders).(map[string]string)
	value, ok := headers[credential.header]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(value)), []byte(credential.secret)) == 1
}
