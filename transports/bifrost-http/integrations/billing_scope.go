package integrations

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/valyala/fasthttp"
)

// billingScopeBodyKeys are top-level request body fields (same placement as "model")
// used for optional user BudgetUsage checks. They are not forwarded to providers.
// billingAccountRef for InferenceUsage is resolved from the VK org's leaf Account.external_id.
var billingScopeBodyKeys = []string{"user_id"}

// stampBillingScopeIDsFromBody reads optional top-level user_id from the JSON body
// (or multipart form fields) and stores it on the Bifrost context for user BudgetUsage checks.
func stampBillingScopeIDsFromBody(bifrostCtx *schemas.BifrostContext, fasthttpCtx *fasthttp.RequestCtx, rawBody []byte) {
	if bifrostCtx == nil {
		return
	}

	userID := billingScopeStringFromBody(rawBody, "user_id")

	// Multipart / form-encoded requests (e.g. audio) may carry the same fields as form values.
	if fasthttpCtx != nil {
		if userID == "" {
			userID = strings.TrimSpace(string(fasthttpCtx.FormValue("user_id")))
		}
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
	// Also strip legacy account_id / contract_id if a client still sends them.
	for _, key := range []string{"account_id", "contract_id"} {
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
