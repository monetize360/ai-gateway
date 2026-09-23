package openai

import (
	"fmt"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const providerErrorBodyLogLimit = 512

// ErrorConverter is a function that converts provider-specific error responses to BifrostError.
type ErrorConverter func(resp *fasthttp.Response) *schemas.BifrostError

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
		// Provider returned a non-standard / empty error payload — try common alternate shapes
		// before falling back to a generic status message.
		if alt := providerUtils.ExtractAlternateProviderErrorMessage(bifrostErr.ExtraFields.RawResponse); alt != "" {
			bifrostErr.Error.Message = alt
		} else {
			bodySnippet := providerUtils.TruncateForLog(providerUtils.RawResponseToString(bifrostErr.ExtraFields.RawResponse), providerErrorBodyLogLimit)
			if bifrostErr.StatusCode != nil {
				if bodySnippet != "" {
					bifrostErr.Error.Message = fmt.Sprintf("provider API error (status %d): %s", *bifrostErr.StatusCode, bodySnippet)
				} else {
					bifrostErr.Error.Message = fmt.Sprintf("provider API error (status %d)", *bifrostErr.StatusCode)
				}
			} else if bodySnippet != "" {
				bifrostErr.Error.Message = fmt.Sprintf("provider API error: %s", bodySnippet)
			} else {
				bifrostErr.Error.Message = "provider API error"
			}
		}
	}

	providerUtils.LogProviderAPIError(resp, bifrostErr)

	return bifrostErr
}
