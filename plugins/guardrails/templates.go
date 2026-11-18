package guardrails

// PolicyTemplate is a prebuilt policy an admin can clone into guardrail_policies.
type PolicyTemplate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Stage       string `json:"stage"`
	Action      string `json:"action"`
	ConfigJSON  string `json:"config_json"`
}

// Templates is empty. The garden reads guardrail_templates in the tenant database.
// A saved policy in guardrail_policies is what the gateway runs.
func Templates() []PolicyTemplate {
	return nil
}
