package integrations

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/valyala/fasthttp"
)

// billingScopeBodyKeys are top-level request body fields (same placement as "model")
// used for governance budget checks. They are not forwarded to providers.
var billingScopeBodyKeys = []string{"account_id", "contract_id", "user_id"}

// stampBillingScopeIDsFromBody reads optional top-level account_id / contract_id / user_id
// from the JSON body (or multipart form fields) and stores them on the Bifrost context.
// Missing values are left unset so PreLLM skips those budget checks.
func stampBillingScopeIDsFromBody(bifrostCtx *schemas.BifrostContext, fasthttpCtx *fasthttp.RequestCtx, rawBody []byte) {
	if bifrostCtx == nil {
		return
	}

	accountID := billingScopeStringFromBody(rawBody, "account_id")
	contractID := billingScopeStringFromBody(rawBody, "contract_id")
	userID := billingScopeStringFromBody(rawBody, "user_id")

	// Multipart / form-encoded requests (e.g. audio) may carry the same fields as form values.
	if fasthttpCtx != nil {
		if accountID == "" {
			accountID = strings.TrimSpace(string(fasthttpCtx.FormValue("account_id")))
		}
		if contractID == "" {
			contractID = strings.TrimSpace(string(fasthttpCtx.FormValue("contract_id")))
		}
		if userID == "" {
			userID = strings.TrimSpace(string(fasthttpCtx.FormValue("user_id")))
		}
	}

	if accountID != "" {
		bifrostCtx.SetValue(schemas.BifrostContextKeyAccountID, accountID)
	}
	if contractID != "" {
		bifrostCtx.SetValue(schemas.BifrostContextKeyContractID, contractID)
	}
	if userID != "" {
		bifrostCtx.SetValue(schemas.BifrostContextKeyBillingUserID, userID)
	}
}

func billingScopeStringFromBody(rawBody []byte, key string) string {
	if len(rawBody) == 0 || key == "" {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(rawBody, key).String())
}

// stripBillingScopeIDsFromBody removes governance billing-scope keys from a raw JSON body
// so providers do not receive unknown fields.
func stripBillingScopeIDsFromBody(rawBody []byte) []byte {
	if len(rawBody) == 0 {
		return rawBody
	}
	out := rawBody
	for _, key := range billingScopeBodyKeys {
		if !gjson.GetBytes(out, key).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(out, key)
		if err != nil {
			continue
		}
		out = next
	}
	return out
}
