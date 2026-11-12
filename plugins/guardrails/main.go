package guardrails

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

const (
	PluginName     = "guardrails"
	defaultHeader  = "x-m360-guardrail-internal"
	optInHeader    = "x-m360-guardrails"
	streamStateKey = "guardrail-stream-state"
)

var errStreamStopped = errors.New("guardrail stopped the stream")

// Config is the built-in plugin configuration.
type Config struct {
	SyncIntervalSeconds int    `json:"sync_interval_seconds"`
	ServiceVirtualKeyID string `json:"service_virtual_key_id"`
	InternalHeader      string `json:"internal_header"`
	ParallelInput       bool   `json:"parallel_input"`
	NemoConfigDir       string `json:"nemo_config_dir"`
	NemoBaseURL         string `json:"nemo_base_url"`
	GatewayBaseURL      string `json:"gateway_base_url"`
}

// Plugin enforces tenant guardrail policies on gateway traffic.
type Plugin struct {
	logger         schemas.Logger
	registry       tenantstore.Resolver
	config         Config
	internalSecret string
	evaluators     *EvaluatorRegistry
	metrics        metrics
	celEnv         *cel.Env
	tenants        sync.Map
	nemoFolders    sync.Map
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// Init builds the guardrails plugin and starts the tenant cache refresh.
func Init(ctx context.Context, config *Config, logger schemas.Logger, registry tenantstore.Resolver) (*Plugin, error) {
	if registry == nil {
		return nil, fmt.Errorf("tenant config registry is required")
	}
	env, err := cel.NewEnv(
		cel.Variable("virtual_key_id", cel.StringType),
		cel.Variable("org_id", cel.StringType),
		cel.Variable("org_unit_id", cel.StringType),
		cel.Variable("provider", cel.StringType),
		cel.Variable("model", cel.StringType),
	)
	if err != nil {
		return nil, err
	}
	cfg := Config{SyncIntervalSeconds: 30, InternalHeader: defaultHeader, GatewayBaseURL: "http://localhost:8082"}
	if config != nil {
		if config.SyncIntervalSeconds > 0 {
			cfg.SyncIntervalSeconds = config.SyncIntervalSeconds
		}
		cfg.ServiceVirtualKeyID = strings.TrimSpace(config.ServiceVirtualKeyID)
		cfg.ParallelInput = config.ParallelInput
		cfg.NemoConfigDir = strings.TrimSpace(config.NemoConfigDir)
		cfg.NemoBaseURL = strings.TrimSpace(config.NemoBaseURL)
		if strings.TrimSpace(config.GatewayBaseURL) != "" {
			cfg.GatewayBaseURL = strings.TrimSpace(config.GatewayBaseURL)
		}
		if strings.TrimSpace(config.InternalHeader) != "" {
			cfg.InternalHeader = strings.ToLower(strings.TrimSpace(config.InternalHeader))
		}
	}
	if envURL := strings.TrimSpace(os.Getenv("NEMO_BASE_URL")); envURL != "" {
		cfg.NemoBaseURL = envURL
	}
	if envDir := strings.TrimSpace(os.Getenv("NEMO_CONFIG_DIR")); envDir != "" {
		cfg.NemoConfigDir = envDir
	}
	if envURL := strings.TrimSpace(os.Getenv("AI_GATEWAY_BASE_URL")); envURL != "" {
		cfg.GatewayBaseURL = envURL
	}
	internalSecret := newInternalSecret()
	schemas.RegisterGuardrailInternalCall(cfg.InternalHeader, internalSecret)
	plugin := &Plugin{
		logger:         logger,
		registry:       registry,
		config:         cfg,
		internalSecret: internalSecret,
		metrics:        newMetrics(),
		celEnv:         env,
	}
	plugin.evaluators = NewEvaluatorRegistry(
		regexEvaluator{},
		newNemoEvaluator(cfg.NemoBaseURL, internalSecret, plugin.nemoFolder),
		newWebhookEvaluator(),
		newLLMJudgeEvaluator(cfg.GatewayBaseURL, internalSecret),
		newBedrockEvaluator(),
		newAzureEvaluator(),
		newLakeraEvaluator(),
		newPresidioEvaluator(),
	)
	runCtx, cancel := context.WithCancel(ctx)
	plugin.cancel = cancel
	plugin.wg.Add(1)
	go plugin.syncLoop(runCtx)
	return plugin, nil
}

func (p *Plugin) GetName() string { return PluginName }

func (p *Plugin) Cleanup() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *Plugin) syncLoop(ctx context.Context) {
	defer p.wg.Done()
	p.reloadAll(ctx)
	ticker := time.NewTicker(time.Duration(p.config.SyncIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reloadAll(ctx)
		}
	}
}

func (p *Plugin) reloadAll(ctx context.Context) {
	if p.registry == nil {
		return
	}
	_ = p.registry.SyncTenants(ctx)
	for _, tenantID := range p.registry.ListTenantIDs(ctx) {
		p.reloadTenant(ctx, tenantID)
	}
}

func (p *Plugin) reloadTenant(ctx context.Context, tenantID string) {
	store := p.registry.PeekStoreForTenant(tenantID)
	if store == nil {
		store = p.registry.GetStoreForTenant(ctx, tenantID)
	}
	if store == nil {
		return
	}
	policies, err := store.GetGuardrailPolicies(ctx)
	if err != nil {
		p.logger.Debug("guardrails: policies unavailable for tenant %s: %v", tenantID, err)
		return
	}
	bindings, err := store.GetGuardrailBindings(ctx)
	if err != nil {
		p.logger.Debug("guardrails: bindings unavailable for tenant %s: %v", tenantID, err)
		return
	}
	cache := p.cache(tenantID)
	cache.replace(
		p.toPolicies(tenantID, policies),
		p.toBindings(bindings),
		p.providerNames(ctx, store),
		p.modelNames(ctx, store),
		p.orgParents(ctx, store),
		p.unitParents(ctx, store),
		p.userUnits(ctx, store),
	)
	if err := p.syncNemoConfigs(ctx, tenantID, store); err != nil {
		p.logger.Debug("guardrails: nemo config sync skipped for tenant %s: %v", tenantID, err)
	}
}

func (p *Plugin) cache(tenantID string) *tenantCache {
	if existing, ok := p.tenants.Load(tenantID); ok {
		return existing.(*tenantCache)
	}
	created := newTenantCache()
	actual, _ := p.tenants.LoadOrStore(tenantID, created)
	return actual.(*tenantCache)
}

func (p *Plugin) toPolicies(tenantID string, rows []tables.TableGuardrailPolicy) map[string]Policy {
	out := make(map[string]Policy, len(rows))
	for _, row := range rows {
		if !row.EnabledValue() || row.Deleted {
			continue
		}
		timeout := 1500
		if row.TimeoutMs != nil && *row.TimeoutMs > 0 {
			timeout = *row.TimeoutMs
		}
		maxTurns := 0
		if row.MaxTurnsToSend != nil {
			maxTurns = *row.MaxTurnsToSend
		}
		chunkChars := 240
		if row.StreamChunkChars != nil && *row.StreamChunkChars > 0 {
			chunkChars = *row.StreamChunkChars
		}
		out[row.ID] = Policy{
			ID:                row.ID,
			TenantID:          tenantID,
			Name:              row.Name,
			Type:              row.TypeName(),
			Stage:             row.StageName(),
			Action:            row.ActionName(),
			StreamMode:        row.StreamModeName(),
			ConfigJSON:        row.ConfigJSON,
			FailClosed:        row.FailClosedValue(),
			TimeoutMs:         timeout,
			MaxTurns:          maxTurns,
			StreamChunkChars:  chunkChars,
			AllowRequestOptIn: row.AllowOptInValue(),
		}
	}
	return out
}

func (p *Plugin) toBindings(rows []tables.TableGuardrailBinding) []Binding {
	out := make([]Binding, 0, len(rows))
	for _, row := range rows {
		if !row.EnabledValue() || row.Deleted {
			continue
		}
		binding := Binding{
			ID:           row.ID,
			PolicyID:     row.GuardrailPolicyID,
			VirtualKeyID: row.VirtualKey(),
			OrgUnitID:    row.OrgUnit(),
			OrgID:        row.Org(),
			Priority:     row.PriorityValue(),
			SamplingRate: row.SamplingRateValue(),
		}
		if row.ProviderID != nil {
			binding.ProviderID = strings.TrimSpace(*row.ProviderID)
		}
		if row.ModelID != nil {
			binding.ModelID = strings.TrimSpace(*row.ModelID)
		}
		if strings.TrimSpace(row.CelExpression) != "" {
			ast, issues := p.celEnv.Compile(row.CelExpression)
			if issues != nil && issues.Err() != nil {
				p.logger.Warn("guardrails: skip binding %s, cel: %v", row.ID, issues.Err())
				continue
			}
			program, err := p.celEnv.Program(ast)
			if err != nil {
				p.logger.Warn("guardrails: skip binding %s, cel program: %v", row.ID, err)
				continue
			}
			binding.Program = program
		}
		out = append(out, binding)
	}
	return out
}

func (p *Plugin) providerNames(ctx context.Context, store configstore.ConfigStore) map[string]string {
	names := map[string]string{}
	providers, err := store.GetProviders(ctx)
	if err != nil {
		return names
	}
	for _, provider := range providers {
		names[provider.ID] = provider.Name
	}
	return names
}

func (p *Plugin) modelNames(ctx context.Context, store configstore.ConfigStore) map[string]modelName {
	names := map[string]modelName{}
	rdb, ok := store.(*configstore.RDBConfigStore)
	if !ok || rdb == nil || rdb.DB() == nil {
		return names
	}
	var rows []tables.TableModel
	if err := rdb.DB().WithContext(ctx).Where("deleted = ?", false).Find(&rows).Error; err != nil {
		return names
	}
	for _, row := range rows {
		names[row.ID] = modelName{Name: row.Name, ProviderID: row.ProviderID}
	}
	return names
}

func (p *Plugin) orgParents(ctx context.Context, store configstore.ConfigStore) map[string]string {
	parents := map[string]string{}
	orgs, err := store.GetOrganizations(ctx)
	if err != nil {
		return parents
	}
	for _, org := range orgs {
		if org.ParentOrgID != nil {
			parents[org.ID] = *org.ParentOrgID
		}
	}
	return parents
}

func (p *Plugin) unitParents(ctx context.Context, store configstore.ConfigStore) map[string]string {
	parents := map[string]string{}
	units, err := store.GetOrgUnits(ctx)
	if err != nil {
		return parents
	}
	for _, unit := range units {
		if unit.ParentID != nil {
			parents[unit.ID] = *unit.ParentID
		}
	}
	return parents
}

func (p *Plugin) userUnits(ctx context.Context, store configstore.ConfigStore) map[string]string {
	units := map[string]string{}
	rows, err := store.GetUserOrgUnits(ctx)
	if err != nil {
		return units
	}
	for _, row := range rows {
		if row.OrgUnitID != nil {
			units[row.ID] = *row.OrgUnitID
		}
	}
	return units
}

func (p *Plugin) skipInternal(ctx *schemas.BifrostContext) bool {
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if headers == nil {
		return false
	}
	value := headerValue(headers, p.config.InternalHeader)
	if p.internalSecret != "" && value == p.internalSecret {
		return true
	}
	if p.config.ServiceVirtualKeyID == "" || !strings.EqualFold(value, "true") {
		return false
	}
	return bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyID) == p.config.ServiceVirtualKeyID
}

func headerValue(headers map[string]string, name string) string {
	if value, ok := headers[name]; ok {
		return strings.TrimSpace(value)
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newInternalSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func (p *Plugin) policiesFor(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, stage string) []Policy {
	if p.skipInternal(ctx) {
		return nil
	}
	tenantID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyTenantID)
	if tenantID == "" {
		return nil
	}
	if _, ok := p.tenants.Load(tenantID); !ok {
		p.reloadTenant(ctx, tenantID)
	}
	provider, model, _ := req.GetRequestFields()
	userID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyUserID)
	if userID == "" {
		userID = bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyBillingUserID)
	}
	cache := p.cache(tenantID)
	scope := cache.scopeFor(
		bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyID),
		bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyGovernanceOrgID),
		userID,
		string(provider),
		model,
	)
	orgUnitID := ""
	if len(scope.OrgUnitIDs) > 0 {
		orgUnitID = scope.OrgUnitIDs[0]
	}
	policies := cache.Resolve(scope, stage, func(rate int) bool {
		if rate >= 100 {
			return true
		}
		if rate <= 0 {
			return false
		}
		return mathrand.Intn(100) < rate
	}, map[string]any{
		"virtual_key_id": scope.VirtualKeyID,
		"org_id":         scope.OrgID,
		"org_unit_id":    orgUnitID,
		"provider":       scope.ProviderName,
		"model":          scope.ModelName,
	})
	return append(policies, cache.OptInPolicies(headerList(ctx, optInHeader), stage, policies)...)
}

func headerList(ctx *schemas.BifrostContext, name string) []string {
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if headers == nil {
		return nil
	}
	raw := ""
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			raw = value
			break
		}
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

func (p *Plugin) recordResult(ctx *schemas.BifrostContext, result StageResult) {
	if result.Decision == "" {
		return
	}
	existing, _ := ctx.Value(schemas.BifrostContextKeyGuardrailDecision).(string)
	decision := result.Decision
	if rank(existing) > rank(decision) {
		decision = existing
	}
	ctx.SetValue(schemas.BifrostContextKeyGuardrailDecision, decision)
	var details []verdictDetail
	if raw, _ := ctx.Value(schemas.BifrostContextKeyGuardrailDetails).(string); raw != "" {
		_ = json.Unmarshal([]byte(raw), &details)
	}
	details = append(details, result.Details...)
	encoded, err := json.Marshal(details)
	if err == nil {
		ctx.SetValue(schemas.BifrostContextKeyGuardrailDetails, string(encoded))
	}
}

func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if req == nil || req.ChatRequest == nil {
		return req, nil, nil
	}
	policies := p.policiesFor(ctx, req, "input")
	if len(policies) == 0 {
		return req, nil, nil
	}
	if p.config.ParallelInput {
		p.startParallelInput(ctx, policies, requestMessages(req))
		return req, nil, nil
	}
	result := p.evaluateStage(ctx, policies, requestMessages(req), "input")
	p.recordResult(ctx, result)
	if result.Decision == DecisionBlock {
		return req, &schemas.LLMPluginShortCircuit{Error: blockError(blockMessage(result))}, nil
	}
	if result.Decision == DecisionRedact {
		applyRequestMessages(req, result.Messages)
	}
	return req, nil, nil
}

func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if inputErr := p.consumeParallelInput(ctx); inputErr != nil {
		return nil, inputErr, nil
	}
	if bifrostErr != nil || resp == nil || resp.ChatResponse == nil {
		return resp, bifrostErr, nil
	}
	provider := string(resp.ChatResponse.ExtraFields.Provider)
	model := resp.ChatResponse.Model
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.ModelProvider(provider), Model: model}}
	policies := p.policiesFor(ctx, req, "output")
	if len(policies) == 0 {
		return resp, bifrostErr, nil
	}
	if isStreamChat(resp) {
		return p.finishStreamChunk(ctx, resp, policies)
	}
	text := responseText(resp)
	result := p.evaluateStage(ctx, policies, []MessageText{{Role: "assistant", Text: text}}, "output")
	p.recordResult(ctx, result)
	if result.Decision == DecisionBlock {
		return nil, blockError(blockMessage(result)), nil
	}
	if result.Decision == DecisionRedact {
		applyResponseText(resp, result.OutputText)
	}
	return resp, bifrostErr, nil
}

func isStreamChat(resp *schemas.BifrostResponse) bool {
	if resp == nil || resp.ChatResponse == nil {
		return false
	}
	for _, choice := range resp.ChatResponse.Choices {
		if choice.ChatStreamResponseChoice != nil {
			return true
		}
	}
	return false
}

func (p *Plugin) finishStreamChunk(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, policies []Policy) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	chunk := &schemas.BifrostStreamChunk{BifrostChatResponse: resp.ChatResponse}
	out, streamErr := p.onStreamChunk(ctx, chunk, policies)
	if streamErr != nil && (out == nil || out.BifrostError == nil) {
		skip := true
		logError := false
		return nil, &schemas.BifrostError{StreamControl: &schemas.StreamControl{SkipStream: &skip, LogError: &logError}}, nil
	}
	if out != nil && out.BifrostError != nil {
		return nil, out.BifrostError, nil
	}
	return resp, nil, nil
}

func (p *Plugin) HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

func (p *Plugin) HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	if resp == nil {
		return nil
	}
	decision, _ := ctx.Value(schemas.BifrostContextKeyGuardrailDecision).(string)
	details, _ := ctx.Value(schemas.BifrostContextKeyGuardrailDetails).(string)
	if decision == "" && details == "" {
		return nil
	}
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	if decision != "" {
		resp.Headers["x-bf-guardrail-decision"] = decision
	}
	if details != "" {
		resp.Headers["x-bf-guardrail-policies"] = details
	}
	return nil
}

func (p *Plugin) HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	buf, _ := ctx.Value(streamStateKey).(*streamBuf)
	if buf == nil {
		return chunk, nil
	}
	buf.mu.Lock()
	blocked := buf.blocked
	buf.mu.Unlock()
	if blocked && (chunk == nil || chunk.BifrostError == nil) {
		return nil, errStreamStopped
	}
	return chunk, nil
}

func blockMessage(result StageResult) string {
	names := strings.Join(result.PolicyNames, ", ")
	if names == "" {
		names = "guardrail"
	}
	return "Blocked by guardrail: " + names
}

func blockError(message string) *schemas.BifrostError {
	status := 400
	errType := "guardrail_violation"
	allow := false
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &status,
		Type:           &errType,
		AllowFallbacks: &allow,
		Error:          &schemas.ErrorField{Type: &errType, Message: message},
	}
}

func blockChunk(message string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostError: blockError(message)}
}
