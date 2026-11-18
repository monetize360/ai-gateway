package guardrails

import (
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// Policy is a guardrail with picklist ids already resolved to logical names.
type Policy struct {
	ID                string
	TenantID          string
	Name              string
	Type              string
	Stage             string
	Action            string
	StreamMode        string
	ConfigJSON        string
	FailClosed        bool
	TimeoutMs         int
	MaxTurns          int
	StreamChunkChars  int
	Priority          int
	AllowRequestOptIn bool
}

func (p Policy) AppliesTo(stage string) bool {
	return p.Stage == stage || p.Stage == tables.GuardrailStageBoth
}

// Binding is one attachment of a policy to a caller and optional provider/model.
type Binding struct {
	ID           string
	PolicyID     string
	VirtualKeyID string
	OrgUnitID    string
	OrgID        string
	ProviderID   string
	ModelID      string
	Priority     int
	SamplingRate int
	Program      cel.Program
}

// RequestScope is who is calling and where the request is going.
type RequestScope struct {
	VirtualKeyID string
	OrgID        string
	OrgUnitIDs   []string
	OrgIDs       []string
	ProviderName string
	ProviderID   string
	ModelName    string
	ModelID      string
}

type modelName struct {
	Name       string
	ProviderID string
}

// tenantCache is the in-memory copy of one tenant's guardrail rows.
type tenantCache struct {
	mu          sync.RWMutex
	policies    map[string]Policy
	bindings    []Binding
	providerIDs map[string]string
	models      map[string]modelName
	orgParent   map[string]string
	unitParent  map[string]string
	userUnit    map[string]string
}

func newTenantCache() *tenantCache {
	return &tenantCache{
		policies:    map[string]Policy{},
		providerIDs: map[string]string{},
		models:      map[string]modelName{},
		orgParent:   map[string]string{},
		unitParent:  map[string]string{},
		userUnit:    map[string]string{},
	}
}

func (c *tenantCache) replace(
	policies map[string]Policy,
	bindings []Binding,
	providerIDs map[string]string,
	models map[string]modelName,
	orgParent, unitParent, userUnit map[string]string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policies = policies
	c.bindings = bindings
	c.providerIDs = providerIDs
	c.models = models
	c.orgParent = orgParent
	c.unitParent = unitParent
	c.userUnit = userUnit
}

func (c *tenantCache) scopeFor(virtualKeyID, orgID, userID, providerName, modelNameValue string) RequestScope {
	c.mu.RLock()
	defer c.mu.RUnlock()
	scope := RequestScope{
		VirtualKeyID: strings.TrimSpace(virtualKeyID),
		OrgID:        strings.TrimSpace(orgID),
		ProviderName: strings.TrimSpace(providerName),
		ModelName:    strings.TrimSpace(modelNameValue),
	}
	if userID != "" {
		if unitID := c.userUnit[userID]; unitID != "" {
			scope.OrgUnitIDs = walkParents(unitID, c.unitParent)
		}
	}
	if scope.OrgID != "" {
		scope.OrgIDs = walkParents(scope.OrgID, c.orgParent)
	}
	for id, name := range c.providerIDs {
		if strings.EqualFold(name, scope.ProviderName) {
			scope.ProviderID = id
			break
		}
	}
	for id, model := range c.models {
		if !strings.EqualFold(model.Name, scope.ModelName) {
			continue
		}
		if scope.ProviderID != "" && model.ProviderID != scope.ProviderID {
			continue
		}
		scope.ModelID = id
		break
	}
	return scope
}

func walkParents(start string, parentOf map[string]string) []string {
	var chain []string
	seen := map[string]bool{}
	for id := strings.TrimSpace(start); id != "" && !seen[id]; {
		seen[id] = true
		chain = append(chain, id)
		id = strings.TrimSpace(parentOf[id])
	}
	return chain
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Resolve returns the policies that apply to this request and stage, highest priority first.
func (c *tenantCache) Resolve(scope RequestScope, stage string, sample func(int) bool, celVars map[string]any) []Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	var policies []Policy
	for _, binding := range c.bindings {
		policy, ok := c.policies[binding.PolicyID]
		if !ok || !policy.AppliesTo(stage) {
			continue
		}
		if !bindingMatches(binding, scope) {
			continue
		}
		if binding.Program != nil {
			out, _, err := binding.Program.Eval(celVars)
			if err != nil {
				continue
			}
			allowed, ok := out.Value().(bool)
			if !ok || !allowed {
				continue
			}
		}
		if !sample(binding.SamplingRate) {
			continue
		}
		if seen[binding.PolicyID] {
			continue
		}
		seen[binding.PolicyID] = true
		policy.Priority = binding.Priority
		policies = append(policies, policy)
	}
	for i := 0; i < len(policies); i++ {
		for j := i + 1; j < len(policies); j++ {
			if policies[j].Priority > policies[i].Priority {
				policies[i], policies[j] = policies[j], policies[i]
			}
		}
	}
	return policies
}

// TestScope builds a scope for a catalog test call.
func (c *tenantCache) TestScope(scopeKind, scopeID, providerName, modelNameValue string) RequestScope {
	c.mu.RLock()
	defer c.mu.RUnlock()
	scope := RequestScope{
		ProviderName: strings.TrimSpace(providerName),
		ModelName:    strings.TrimSpace(modelNameValue),
	}
	for id, name := range c.providerIDs {
		if strings.EqualFold(name, scope.ProviderName) {
			scope.ProviderID = id
			break
		}
	}
	for id, model := range c.models {
		if !strings.EqualFold(model.Name, scope.ModelName) {
			continue
		}
		if scope.ProviderID != "" && model.ProviderID != scope.ProviderID {
			continue
		}
		scope.ModelID = id
		break
	}
	switch strings.TrimSpace(scopeKind) {
	case "virtual-keys":
		scope.VirtualKeyID = strings.TrimSpace(scopeID)
	case "organizations":
		scope.OrgID = strings.TrimSpace(scopeID)
		scope.OrgIDs = walkParents(scope.OrgID, c.orgParent)
	case "org-units":
		scope.OrgUnitIDs = walkParents(strings.TrimSpace(scopeID), c.unitParent)
	}
	return scope
}

// PolicyByID returns one enabled policy from the tenant cache.
func (c *tenantCache) PolicyByID(id string) (Policy, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	policy, ok := c.policies[id]
	return policy, ok
}

// OptInPolicies adds policies named by the caller when those policies allow request opt-in.
func (c *tenantCache) OptInPolicies(names []string, stage string, already []Policy) []Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	for _, policy := range already {
		seen[policy.ID] = true
	}
	var extra []Policy
	for _, name := range names {
		wanted := strings.TrimSpace(name)
		if wanted == "" {
			continue
		}
		for _, policy := range c.policies {
			if !strings.EqualFold(policy.Name, wanted) || seen[policy.ID] || !policy.AllowRequestOptIn || !policy.AppliesTo(stage) {
				continue
			}
			seen[policy.ID] = true
			extra = append(extra, policy)
		}
	}
	return extra
}

func bindingMatches(binding Binding, scope RequestScope) bool {
	switch {
	case binding.VirtualKeyID != "":
		if binding.VirtualKeyID != scope.VirtualKeyID {
			return false
		}
	case binding.OrgUnitID != "":
		if !contains(scope.OrgUnitIDs, binding.OrgUnitID) {
			return false
		}
	case binding.OrgID != "":
		if !contains(scope.OrgIDs, binding.OrgID) {
			return false
		}
	}
	if binding.ProviderID != "" && binding.ProviderID != scope.ProviderID {
		return false
	}
	if binding.ModelID != "" && binding.ModelID != scope.ModelID {
		return false
	}
	return true
}
