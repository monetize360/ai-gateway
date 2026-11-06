package guardrails

import (
	"context"
	"errors"
	"strings"
)

var (
	errMissingTenant = errors.New("tenant is required")
	errUnknownPolicy = errors.New("guardrail policy was not found")
	errStageMismatch = errors.New("policy does not apply to this stage")
)

// TestRequest runs one policy, or the policies for a scope, against sample text.
type TestRequest struct {
	PolicyID string        `json:"policy_id"`
	Stage    string        `json:"stage"`
	Scope    string        `json:"scope"`
	ScopeID  string        `json:"scope_id"`
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Text     string        `json:"text"`
	Messages []MessageText `json:"messages"`
	// VirtualKeyToken stands in for the caller's bearer token. A test call has no incoming
	// request, so a policy that asks a model uses this key for that call.
	VirtualKeyToken string `json:"virtual_key_token"`
}

// TestResult is the verdict returned by the test call.
type TestResult struct {
	Decision string          `json:"decision"`
	Policies []string        `json:"policies"`
	Details  []verdictDetail `json:"details"`
	Output   string          `json:"output"`
}

// Types returns the registered evaluator catalog.
func (p *Plugin) Types() []TypeInfo {
	if p == nil || p.evaluators == nil {
		return nil
	}
	return p.evaluators.List()
}

// PolicyTemplates returns the shipped templates.
func (p *Plugin) PolicyTemplates() []PolicyTemplate {
	return Templates()
}

// Test evaluates sample text without calling the user's model.
func (p *Plugin) Test(ctx context.Context, tenantID string, request TestRequest) (TestResult, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return TestResult{}, errMissingTenant
	}
	if _, ok := p.tenants.Load(tenantID); !ok {
		p.reloadTenant(ctx, tenantID)
	}
	stage := strings.TrimSpace(request.Stage)
	if stage == "" {
		stage = "input"
	}
	messages := request.Messages
	if len(messages) == 0 {
		role := "user"
		if stage == "output" {
			role = "assistant"
		}
		messages = []MessageText{{Role: role, Text: request.Text}}
	}
	cache := p.cache(tenantID)
	policies, err := policiesForTest(cache, request, stage)
	if err != nil {
		return TestResult{}, err
	}
	result := p.evaluateStage(withCallerToken(ctx, request.VirtualKeyToken), policies, messages, stage)
	return TestResult{
		Decision: result.Decision,
		Policies: result.PolicyNames,
		Details:  result.Details,
		Output:   result.OutputText,
	}, nil
}

func policiesForTest(cache *tenantCache, request TestRequest, stage string) ([]Policy, error) {
	if id := strings.TrimSpace(request.PolicyID); id != "" {
		policy, ok := cache.PolicyByID(id)
		if !ok {
			return nil, errUnknownPolicy
		}
		if !policy.AppliesTo(stage) {
			return nil, errStageMismatch
		}
		return []Policy{policy}, nil
	}
	scope := cache.TestScope(request.Scope, request.ScopeID, request.Provider, request.Model)
	return cache.Resolve(scope, stage, func(int) bool { return true }, map[string]any{
		"virtual_key_id": scope.VirtualKeyID,
		"org_id":         scope.OrgID,
		"org_unit_id":    firstID(scope.OrgUnitIDs),
		"provider":       scope.ProviderName,
		"model":          scope.ModelName,
	}), nil
}

func firstID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
