package openai

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestParseOpenAIError_FallbackMessageWhenProviderBodyIsNonOpenAIShape(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusUnprocessableEntity)
	resp.SetBodyString(`{"detail":[{"loc":["body","messages",0,"role"],"msg":"value is not a valid enumeration member"}]}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	// FastAPI-style detail[0].msg is extracted when OpenAI error.message is absent.
	if errResp.Error.Message != "value is not a valid enumeration member" {
		t.Fatalf("expected alternate detail message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_PreservesProviderMessageWhenPresent(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusUnprocessableEntity)
	resp.SetBodyString(`{"error":{"message":"unsupported role: developer","type":"invalid_request_error","param":"messages.0.role","code":"invalid_value"}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if errResp.Error.Message != "unsupported role: developer" {
		t.Fatalf("expected provider message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_FallbackMessageWhenBodyIsEmpty(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBody(nil)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	// HandleProviderAPIError returns ErrProviderResponseEmpty with HTTP status for empty bodies.
	expectedMsg := schemas.ErrProviderResponseEmpty + " (HTTP 400)"
	if errResp.Error.Message != expectedMsg {
		t.Fatalf("expected %q, got %q", expectedMsg, errResp.Error.Message)
	}
}

func TestParseOpenAIError_WhitespaceProviderMessageFallsBack(t *testing.T) {
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusBadRequest)
	resp.SetBodyString(`{"error":{"message":"   ","type":"invalid_request_error"}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if !strings.HasPrefix(errResp.Error.Message, "provider API error (status 400)") {
		t.Fatalf("expected fallback message prefix, got %q", errResp.Error.Message)
	}
	if !strings.Contains(errResp.Error.Message, `"type":"invalid_request_error"`) {
		t.Fatalf("expected truncated provider body in fallback message, got %q", errResp.Error.Message)
	}
}

func TestParseOpenAIError_DefaultStatusCodeFallsBackWithStatusNumber(t *testing.T) {
	var resp fasthttp.Response
	// fasthttp defaults zero-value response status code to 200.
	resp.SetBodyString(`{"error":{"message":""}}`)

	errResp := ParseOpenAIError(&resp)
	if errResp == nil || errResp.Error == nil {
		t.Fatal("expected non-nil error response")
	}
	if !strings.HasPrefix(errResp.Error.Message, "provider API error (status 200)") {
		t.Fatalf("expected fallback message with default status, got %q", errResp.Error.Message)
	}
}
