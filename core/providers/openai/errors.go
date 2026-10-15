package openai

import (
	"fmt"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ErrorConverter is a function that converts provider-specific error responses to BifrostError.
type ErrorConverter func(resp *fasthttp.Response) *schemas.BifrostError

const providerErrorBodyLogLimit = 2048

// ParseOpenAIError parses OpenAI error responses.
func ParseOpenAIError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp schemas.BifrostError

	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	if errorResp.EventID != nil {
		bifrostErr.EventID = errorResp.EventID
	}

	if errorResp.Error != nil {
		if bifrostErr.Error == nil {
			bifrostErr.Error = &schemas.ErrorField{}
		}
		bifrostErr.Error.Type = errorResp.Error.Type
		bifrostErr.Error.Code = errorResp.Error.Code
		if errorResp.Error.Message != "" {
			bifrostErr.Error.Message = errorResp.Error.Message
		}
		bifrostErr.Error.Param = errorResp.Error.Param
		if errorResp.Error.EventID != nil {
			bifrostErr.Error.EventID = errorResp.Error.EventID
		}
	}

	if bifrostErr.Error == nil {
		bifrostErr.Error = &schemas.ErrorField{}
	}
	if strings.TrimSpace(bifrostErr.Error.Message) == "" {
		if bifrostErr.StatusCode != nil {
			bifrostErr.Error.Message = fmt.Sprintf("provider API error (status %d)", *bifrostErr.StatusCode)
		} else {
			bifrostErr.Error.Message = "provider API error"
		}
	}

	enrichOpaqueProviderErrorMessage(bifrostErr, resp)
	return bifrostErr
}

// enrichOpaqueProviderErrorMessage appends upstream HTTP status + truncated body
// when the provider error text is too generic to debug on its own.
func enrichOpaqueProviderErrorMessage(bifrostErr *schemas.BifrostError, resp *fasthttp.Response) {
	if bifrostErr == nil || bifrostErr.Error == nil || resp == nil {
		return
	}
	msg := strings.TrimSpace(bifrostErr.Error.Message)
	if !isOpaqueProviderErrorMessage(msg) {
		return
	}
	raw := providerErrorBodyPreview(resp, providerErrorBodyLogLimit)
	status := resp.StatusCode()
	if bifrostErr.StatusCode != nil {
		status = *bifrostErr.StatusCode
	}
	if raw == "" {
		bifrostErr.Error.Message = fmt.Sprintf("%s (upstream_http=%d; empty body)", msg, status)
		return
	}
	bifrostErr.Error.Message = fmt.Sprintf("%s (upstream_http=%d; body=%s)", msg, status, raw)
}

func isOpaqueProviderErrorMessage(msg string) bool {
	switch strings.ToLower(strings.TrimSpace(msg)) {
	case "internal server error", "internal server error.", "provider api error",
		"an internal error has occurred", "an internal error occurred":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(msg), "provider api error (status ")
	}
}

func vkNameFromCtx(ctx *schemas.BifrostContext) string {
	if ctx == nil {
		return "(none)"
	}
	if name, ok := ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyName).(string); ok && name != "" {
		return name
	}
	return "(none)"
}

func logRequestFlow(ctx *schemas.BifrostContext, logger schemas.Logger, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Info(fmt.Sprintf("[request-flow] vk=%q ", vkNameFromCtx(ctx))+format, args...)
}

func logRequestFlowWarn(ctx *schemas.BifrostContext, logger schemas.Logger, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Warn(fmt.Sprintf("[request-flow] vk=%q ", vkNameFromCtx(ctx))+format, args...)
}

// logProviderChatHTTPError writes a structured Warn line for non-2xx OpenAI-compatible
// chat responses so gateway failures are visible at default log level.
func logProviderChatHTTPError(
	ctx *schemas.BifrostContext,
	logger schemas.Logger,
	model string,
	resp *fasthttp.Response,
) {
	if logger == nil || resp == nil {
		return
	}
	msg := providerErrorBodyPreview(resp, 256)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode())
	}
	logRequestFlowWarn(ctx, logger, "6/llm-error: LLM error status=%d message=%q", resp.StatusCode(), msg)
}

// logProviderChatUpstreamCall logs the outbound OpenAI-compatible call and the
// immediate HTTP response status (before SSE body is consumed).
func logProviderChatUpstreamCall(
	ctx *schemas.BifrostContext,
	logger schemas.Logger,
	request *schemas.BifrostChatRequest,
	stream bool,
	phase string,
	resp *fasthttp.Response,
	callErr error,
) {
	if logger == nil {
		return
	}
	model := ""
	if request != nil {
		model = request.Model
	}
	switch phase {
	case "request":
		logRequestFlow(ctx, logger, "4/calling-llm: calling LLM API for model=%q stream=%t", model, stream)
	case "response":
		status := 0
		if resp != nil {
			status = resp.StatusCode()
		}
		if callErr != nil {
			logRequestFlowWarn(ctx, logger, "6/llm-error: LLM error status=%d message=%q", status, callErr.Error())
			return
		}
		logRequestFlow(ctx, logger, "5/llm-response: got LLM response status=%d", status)
	}
}

func providerErrorBodyPreview(resp *fasthttp.Response, limit int) string {
	if resp == nil {
		return ""
	}
	decoded, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil || len(decoded) == 0 {
		raw := resp.Body()
		if len(raw) == 0 {
			return ""
		}
		decoded = raw
	}
	text := strings.TrimSpace(string(decoded))
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", "")
	if limit > 0 && len(text) > limit {
		return text[:limit] + "...(truncated)"
	}
	return text
}

func chatRequestImageSummary(request *schemas.BifrostChatRequest) (hasImage bool, imageParts int) {
	if request == nil {
		return false, 0
	}
	for i := range request.Input {
		msg := &request.Input[i]
		if msg.Content == nil {
			continue
		}
		for _, part := range msg.Content.ContentBlocks {
			if part.ImageURLStruct != nil || part.Type == schemas.ChatContentBlockTypeImage {
				hasImage = true
				imageParts++
			}
		}
	}
	return hasImage, imageParts
}
