package governance

import "strings"

// Catalogue sentences returned to API callers. The detailed governance reason
// stays in the log.
const (
	MissingAPIKeyMessage                  = "API key is required. Please provide a valid API key."
	InvalidAPIKeyMessage                  = "Invalid API key. Please verify your credentials and try again."
	RevokedAPIKeyMessage                  = "This API key has been revoked. Please generate a new API key."
	ModelNameRequiredMessage              = "Model name is required. Please specify a valid model."
	InvalidModelNameMessage               = "The requested model could not be found. Please verify the model name."
	ModelNotEnabledForOrganisationMessage = "This model is not available for your organisation. Please contact your Organisation Admin."
	ModelRestrictedByDepartmentMessage    = "Access to this model is restricted by your department's settings."
	ModelNotPermittedMessage              = "You don't have permission to access this model. Please contact your Organisation Admin."
	InsufficientWalletBalanceMessage      = "Insufficient wallet balance. Please add credits to continue making API requests."
	DepartmentSpendLimitMessage           = "Department spending limit reached. Please contact your Organisation Admin."
	RateLimitExceededMessage              = "Too many requests. Please wait and try again."
	MissingPromptMessage                  = "Request input is missing. Please provide a valid prompt."
)

// ClientMessage maps a governance evaluation result to the catalogue sentence when one
// matches. Decisions the catalogue does not name keep their existing reason. Model denials
// are classified by the structured ModelDenialScope, never by the reason text, because the
// reason embeds the model name.
func ClientMessage(result *EvaluationResult) string {
	switch result.Decision {
	case DecisionVirtualKeyNotFound:
		return InvalidAPIKeyMessage
	case DecisionVirtualKeyBlocked:
		if strings.Contains(strings.ToLower(result.Reason), "inactive") {
			return RevokedAPIKeyMessage
		}
		return result.Reason
	case DecisionModelBlocked:
		switch result.ModelDenialScope {
		case ModelDenialScopeOrganizationalUnit:
			return ModelRestrictedByDepartmentMessage
		case ModelDenialScopeOrganization:
			return ModelNotEnabledForOrganisationMessage
		case ModelDenialScopeUser, ModelDenialScopeVirtualKey:
			return ModelNotPermittedMessage
		default:
			return result.Reason
		}
	case DecisionWalletInsufficient:
		return InsufficientWalletBalanceMessage
	case DecisionBudgetExceeded:
		if strings.Contains(result.Reason, "Org-unit budget") {
			return DepartmentSpendLimitMessage
		}
		return result.Reason
	case DecisionRateLimited, DecisionTokenLimited, DecisionRequestLimited:
		return RateLimitExceededMessage
	default:
		return result.Reason
	}
}
