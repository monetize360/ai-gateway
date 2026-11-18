package tables

import "strings"

// Guardrail picklist category and item IDs. Keep in sync with
// standardmetadata/picklists/billing/guardrail_*.json.
const (
	GuardrailTypeCategoryID     = "a7c1e4b2-6d30-4f8a-9c11-111111111101"
	GuardrailTypeRegexKeywordID = "a7c1e4b2-6d30-4f8a-9c11-111111111111"
	GuardrailTypeNemoID         = "a7c1e4b2-6d30-4f8a-9c11-111111111112"

	GuardrailStageCategoryID = "a7c1e4b2-6d30-4f8a-9c11-222222222201"
	GuardrailStageInputID    = "a7c1e4b2-6d30-4f8a-9c11-222222222211"
	GuardrailStageOutputID   = "a7c1e4b2-6d30-4f8a-9c11-222222222212"
	GuardrailStageBothID     = "a7c1e4b2-6d30-4f8a-9c11-222222222213"

	GuardrailActionCategoryID = "a7c1e4b2-6d30-4f8a-9c11-333333333301"
	GuardrailActionBlockID    = "a7c1e4b2-6d30-4f8a-9c11-333333333311"
	GuardrailActionRedactID   = "a7c1e4b2-6d30-4f8a-9c11-333333333312"
	GuardrailActionFlagID     = "a7c1e4b2-6d30-4f8a-9c11-333333333313"

	GuardrailStreamModeCategoryID = "a7c1e4b2-6d30-4f8a-9c11-444444444401"
	GuardrailStreamModeChunkedID  = "a7c1e4b2-6d30-4f8a-9c11-444444444411"
	GuardrailStreamModeBufferedID = "a7c1e4b2-6d30-4f8a-9c11-444444444412"
)

const (
	GuardrailTypeRegexKeyword = "regex_keyword"
	GuardrailTypeNemo         = "nemo"

	GuardrailStageInput  = "input"
	GuardrailStageOutput = "output"
	GuardrailStageBoth   = "both"

	GuardrailActionBlock  = "block"
	GuardrailActionRedact = "redact"
	GuardrailActionFlag   = "flag"

	GuardrailStreamModeChunked  = "chunked"
	GuardrailStreamModeBuffered = "buffered"
)

func guardrailName(id string, byID map[string]string) string {
	name, ok := byID[strings.TrimSpace(id)]
	if !ok {
		return strings.TrimSpace(id)
	}
	return name
}

// GuardrailTypeName maps a picklist item id (or an already-logical name) to regex_keyword or nemo.
func GuardrailTypeName(value string) string {
	return guardrailName(value, map[string]string{
		GuardrailTypeRegexKeywordID: GuardrailTypeRegexKeyword,
		GuardrailTypeNemoID:         GuardrailTypeNemo,
	})
}

// GuardrailStageName maps a picklist item id to input, output, or both.
func GuardrailStageName(value string) string {
	return guardrailName(value, map[string]string{
		GuardrailStageInputID:  GuardrailStageInput,
		GuardrailStageOutputID: GuardrailStageOutput,
		GuardrailStageBothID:   GuardrailStageBoth,
	})
}

// GuardrailActionName maps a picklist item id to block, redact, or flag.
func GuardrailActionName(value string) string {
	return guardrailName(value, map[string]string{
		GuardrailActionBlockID:  GuardrailActionBlock,
		GuardrailActionRedactID: GuardrailActionRedact,
		GuardrailActionFlagID:   GuardrailActionFlag,
	})
}

// GuardrailStreamModeName maps a picklist item id to chunked or buffered.
func GuardrailStreamModeName(value string) string {
	return guardrailName(value, map[string]string{
		GuardrailStreamModeChunkedID:  GuardrailStreamModeChunked,
		GuardrailStreamModeBufferedID: GuardrailStreamModeBuffered,
	})
}
