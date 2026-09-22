package governance

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticrouter"
	"github.com/valyala/fasthttp"
)

const (
	defaultVLLMSRPreviewPath = "/api/v1/routing/preview"
	defaultVLLMSREntrypoint  = "vllm-sr/auto"
	defaultVLLMSRTimeoutMs   = 2000
	vllmsrMessagesMaxChars   = 8000
	vllmsrMediaRefMaxChars   = 256

	vllmsrBreakerThreshold = 3
	vllmsrBreakerCooldown  = 15 * time.Second
	vllmsrMaxConnsPerHost  = 64
	vllmsrDialTimeout      = 2 * time.Second
)

const (
	routerModeHTTP   = "http"
	routerModePlugin = "plugin"
)

// VLLMSRRouterConfig is the Layer 2 decision surface.
// Mode "http" calls the vLLM-SR management-API sidecar; mode "plugin" calls
// the in-process semantic-router Bifrost plugin. Inference still goes through Bifrost.
type VLLMSRRouterConfig struct {
	// Mode is "plugin" (in-process) or "http" (sidecar). Empty resolves from
	// other fields: base_url set → http; otherwise plugin when a Layer2 is injected.
	Mode        string `json:"mode,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	PreviewPath string `json:"preview_path,omitempty"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	TimeoutMs   int    `json:"timeout_ms,omitempty"`
}

func (c *VLLMSRRouterConfig) resolvedMode() string {
	if c == nil {
		return ""
	}
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	switch mode {
	case routerModeHTTP, routerModePlugin:
		return mode
	}
	if strings.TrimSpace(c.BaseURL) != "" {
		return routerModeHTTP
	}
	return routerModePlugin
}

func (c *VLLMSRRouterConfig) httpEnabled() bool {
	return c != nil && c.resolvedMode() == routerModeHTTP && strings.TrimSpace(c.BaseURL) != ""
}

func (c *VLLMSRRouterConfig) enabled() bool {
	// Backward-compatible: any non-empty base_url still means "router configured".
	// Plugin mode is enabled when mode resolves to plugin (Layer2 presence checked separately).
	if c == nil {
		return false
	}
	switch c.resolvedMode() {
	case routerModeHTTP:
		return strings.TrimSpace(c.BaseURL) != ""
	case routerModePlugin:
		return true
	default:
		return strings.TrimSpace(c.BaseURL) != ""
	}
}

func (c *VLLMSRRouterConfig) timeout() time.Duration {
	if c != nil && c.TimeoutMs > 0 {
		return time.Duration(c.TimeoutMs) * time.Millisecond
	}
	return time.Duration(defaultVLLMSRTimeoutMs) * time.Millisecond
}

func (c *VLLMSRRouterConfig) previewURL() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	path := strings.TrimSpace(c.PreviewPath)
	if path == "" {
		path = defaultVLLMSRPreviewPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func (c *VLLMSRRouterConfig) entrypoint() string {
	if c != nil && strings.TrimSpace(c.Entrypoint) != "" {
		return strings.TrimSpace(c.Entrypoint)
	}
	return defaultVLLMSREntrypoint
}

func (c *SemanticRoutingConfig) routerEnabled() bool {
	// Config-level: router block is present and mode/base_url resolve to a usable mode.
	// Runtime Layer 2 availability also needs the plugin injection (see GovernancePlugin.layer2Enabled).
	return c != nil && c.Router != nil && c.Router.enabled()
}

func (c *SemanticRoutingConfig) routerMode() string {
	if c == nil || c.Router == nil {
		return ""
	}
	return c.Router.resolvedMode()
}

func (c *SemanticRoutingConfig) usesHTTPRouter() bool {
	return c != nil && c.Router != nil && c.Router.resolvedMode() == routerModeHTTP && c.Router.httpEnabled()
}

func (c *SemanticRoutingConfig) usesPluginRouter() bool {
	return c != nil && c.Router != nil && c.Router.resolvedMode() == routerModePlugin
}

// vllmsrRoute is the Layer 2 result used to order models from the virtual-key pool.
type vllmsrRoute struct {
	SelectedModel   string
	Decision        string
	Algorithm       string
	SelectionStatus string
	SelectionMethod string
	Candidates      []string
}

func (r *vllmsrRoute) skipRewrite() bool {
	if r == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(r.SelectionStatus))
	method := strings.ToLower(strings.TrimSpace(r.SelectionMethod))
	return status == "not_required" || method == "fast_response"
}

// semanticRouteResult labels Layer 2 outcomes for counters / logs.
type semanticRouteResult string

const (
	semanticRouteSelected semanticRouteResult = "selected"
	semanticRouteSkip     semanticRouteResult = "skip"
	semanticRouteFailOpen semanticRouteResult = "fail_open"
	semanticRouteTimeout  semanticRouteResult = "timeout"
	semanticRouteOpen     semanticRouteResult = "circuit_open"
)

type vllmsrCircuitBreaker struct {
	failures  atomic.Int32
	openUntil atomic.Int64 // unix nano; 0 = closed
}

func (b *vllmsrCircuitBreaker) allow() bool {
	if b == nil {
		return true
	}
	until := b.openUntil.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() >= until {
		b.openUntil.Store(0)
		b.failures.Store(0)
		return true
	}
	return false
}

func (b *vllmsrCircuitBreaker) recordSuccess() {
	if b == nil {
		return
	}
	b.failures.Store(0)
	b.openUntil.Store(0)
}

func (b *vllmsrCircuitBreaker) recordFailure() {
	if b == nil {
		return
	}
	n := b.failures.Add(1)
	if n >= vllmsrBreakerThreshold {
		b.openUntil.Store(time.Now().Add(vllmsrBreakerCooldown).UnixNano())
	}
}

func (p *GovernancePlugin) initVLLMSRTransport() {
	p.vllmsrClient = &fasthttp.Client{
		Name:                "bifrost-governance-vllm-sr",
		MaxConnsPerHost:     vllmsrMaxConnsPerHost,
		MaxIdleConnDuration: 30 * time.Second,
		ReadTimeout:         0, // per-call DoTimeout bounds the wait
		WriteTimeout:        vllmsrDialTimeout,
		DialDualStack:       true,
	}
	p.vllmsrBreaker = &vllmsrCircuitBreaker{}
}

func (p *GovernancePlugin) vllmsrHTTPClient() *fasthttp.Client {
	if p != nil && p.vllmsrClient != nil {
		return p.vllmsrClient
	}
	return &fasthttp.Client{MaxConnsPerHost: vllmsrMaxConnsPerHost}
}

func (p *GovernancePlugin) recordSemanticRoute(result semanticRouteResult, latency time.Duration) {
	if p == nil {
		return
	}
	switch result {
	case semanticRouteSelected:
		p.semanticRouteSelected.Add(1)
	case semanticRouteSkip:
		p.semanticRouteSkip.Add(1)
	case semanticRouteFailOpen, semanticRouteOpen:
		p.semanticRouteFailOpen.Add(1)
	case semanticRouteTimeout:
		p.semanticRouteTimeout.Add(1)
		p.semanticRouteFailOpen.Add(1)
	}
	if latency > 0 {
		p.semanticRoutePreviewNs.Add(uint64(latency.Nanoseconds()))
		p.semanticRoutePreviewN.Add(1)
	}
}

// SemanticRouteStats returns cumulative Layer 2 counters (for tests / ops hooks).
func (p *GovernancePlugin) SemanticRouteStats() (selected, skip, failOpen, timeout uint64, avgPreview time.Duration) {
	if p == nil {
		return 0, 0, 0, 0, 0
	}
	selected = p.semanticRouteSelected.Load()
	skip = p.semanticRouteSkip.Load()
	failOpen = p.semanticRouteFailOpen.Load()
	timeout = p.semanticRouteTimeout.Load()
	n := p.semanticRoutePreviewN.Load()
	if n > 0 {
		avgPreview = time.Duration(p.semanticRoutePreviewNs.Load() / n)
	}
	return selected, skip, failOpen, timeout, avgPreview
}

// previewLayer2Route runs Layer 2 via in-process plugin or HTTP sidecar.
func (p *GovernancePlugin) previewLayer2Route(ctx *schemas.BifrostContext, body map[string]any, profile *RequestProfile, pool []routeCandidate, cfg *SemanticRoutingConfig) (*vllmsrRoute, error) {
	if cfg == nil || cfg.Router == nil {
		return nil, fmt.Errorf("layer2 router unset")
	}
	switch cfg.Router.resolvedMode() {
	case routerModePlugin:
		return p.previewPluginRoute(ctx, body, profile, pool, cfg)
	default:
		return p.previewVLLMSRRoute(ctx, body, profile, pool, cfg)
	}
}

// previewPluginRoute calls the in-process semantic-router plugin.
func (p *GovernancePlugin) previewPluginRoute(ctx *schemas.BifrostContext, body map[string]any, profile *RequestProfile, pool []routeCandidate, cfg *SemanticRoutingConfig) (*vllmsrRoute, error) {
	_ = profile
	_ = pool
	if p == nil || p.semanticLayer2 == nil {
		return nil, fmt.Errorf("semantic-router plugin not injected")
	}
	in := semanticrouter.PreviewInput{
		Model:               cfg.Router.entrypoint(),
		Messages:            extractChatMessages(body),
		Text:                stringValue(body["text"]),
		Tools:               body["tools"],
		Functions:           body["functions"],
		ToolChoice:          body["tool_choice"],
		FunctionCall:        body["function_call"],
		ResponseFormat:      body["response_format"],
		MaxTokens:           body["max_tokens"],
		MaxCompletionTokens: body["max_completion_tokens"],
		Metadata:            stringMap(body["metadata"]),
		PreviewContext:      anyMap(body["preview_context"]),
	}
	p.logSemantic(ctx, schemas.LogLevelInfo, "2/route: calling semantic-router plugin mode=plugin")
	start := time.Now()
	previewCtx, cancel := context.WithTimeout(ctx, cfg.Router.timeout())
	defer cancel()
	out, err := p.semanticLayer2.Preview(previewCtx, in)
	latency := time.Since(start)
	if err != nil {
		if isTimeoutErr(err) {
			p.recordSemanticRoute(semanticRouteTimeout, latency)
		} else {
			p.recordSemanticRoute(semanticRouteFailOpen, latency)
		}
		p.logSemantic(ctx, schemas.LogLevelWarn,
			"2/route: plugin preview error took=%s err=%v", formatTook(latency), err)
		return nil, fmt.Errorf("semantic-router plugin preview: %w", err)
	}
	if out == nil {
		p.recordSemanticRoute(semanticRouteFailOpen, latency)
		return nil, fmt.Errorf("semantic-router plugin returned nil preview")
	}
	route := &vllmsrRoute{
		SelectedModel:   out.SelectedModel,
		Decision:        out.Decision,
		Algorithm:       out.Algorithm,
		SelectionStatus: out.SelectionStatus,
		SelectionMethod: out.SelectionMethod,
		Candidates:      append([]string(nil), out.Candidates...),
	}
	if route.skipRewrite() {
		p.recordSemanticRoute(semanticRouteSkip, latency)
	} else {
		if strings.TrimSpace(route.SelectedModel) == "" {
			p.recordSemanticRoute(semanticRouteFailOpen, latency)
			return nil, fmt.Errorf("semantic-router plugin returned empty selected_model")
		}
		p.recordSemanticRoute(semanticRouteSelected, latency)
	}
	p.logSemantic(ctx, schemas.LogLevelInfo,
		"2/route: plugin response took=%s selected=%s recommended=%v decision=%q algorithm=%q status=%q method=%q",
		formatTook(latency), route.SelectedModel, route.Candidates, route.Decision, route.Algorithm, route.SelectionStatus, route.SelectionMethod)
	return route, nil
}

// previewVLLMSRRoute calls the vLLM-SR management API for the Layer 2 decision.
func (p *GovernancePlugin) previewVLLMSRRoute(ctx *schemas.BifrostContext, body map[string]any, profile *RequestProfile, pool []routeCandidate, cfg *SemanticRoutingConfig) (*vllmsrRoute, error) {
	if cfg == nil || !cfg.Router.httpEnabled() {
		return nil, fmt.Errorf("vllm-sr router base_url unset")
	}
	if p != nil && p.vllmsrBreaker != nil && !p.vllmsrBreaker.allow() {
		p.recordSemanticRoute(semanticRouteOpen, 0)
		return nil, fmt.Errorf("vllm-sr circuit open")
	}

	payload, err := sonic.Marshal(buildVLLMSRPreviewRequest(body, profile, pool, cfg.Router))
	if err != nil {
		return nil, fmt.Errorf("marshal vllm-sr preview: %w", err)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	previewURL := cfg.Router.previewURL()
	req.SetRequestURI(previewURL)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetContentType("application/json")
	if key := strings.TrimSpace(cfg.Router.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.SetBody(payload)

	p.logSemantic(ctx, schemas.LogLevelInfo,
		"2/route: calling vllm-sr url=%s payload_bytes=%d", previewURL, len(payload))

	start := time.Now()
	err = p.vllmsrHTTPClient().DoTimeout(req, resp, cfg.Router.timeout())
	latency := time.Since(start)
	if err != nil {
		if p != nil && p.vllmsrBreaker != nil {
			p.vllmsrBreaker.recordFailure()
		}
		if isTimeoutErr(err) {
			p.recordSemanticRoute(semanticRouteTimeout, latency)
		} else {
			p.recordSemanticRoute(semanticRouteFailOpen, latency)
		}
		p.logSemantic(ctx, schemas.LogLevelWarn,
			"2/route: vllm-sr response error took=%s err=%v", formatTook(latency), err)
		return nil, fmt.Errorf("vllm-sr preview http: %w", err)
	}
	if code := resp.StatusCode(); code < 200 || code >= 300 {
		if p != nil && p.vllmsrBreaker != nil {
			p.vllmsrBreaker.recordFailure()
		}
		p.recordSemanticRoute(semanticRouteFailOpen, latency)
		bodySnippet := truncateRunes(string(resp.Body()), 200)
		p.logSemantic(ctx, schemas.LogLevelWarn,
			"2/route: vllm-sr response error took=%s http_status=%d body=%q",
			formatTook(latency), code, bodySnippet)
		return nil, fmt.Errorf("vllm-sr preview http status %d: %s", code, bodySnippet)
	}

	route, err := parseVLLMSRPreview(resp.Body())
	if err != nil {
		if p != nil && p.vllmsrBreaker != nil {
			p.vllmsrBreaker.recordFailure()
		}
		p.recordSemanticRoute(semanticRouteFailOpen, latency)
		p.logSemantic(ctx, schemas.LogLevelWarn,
			"2/route: vllm-sr response decode error took=%s err=%v body=%q",
			formatTook(latency), err, truncateRunes(string(resp.Body()), 200))
		return nil, err
	}
	if p != nil && p.vllmsrBreaker != nil {
		p.vllmsrBreaker.recordSuccess()
	}
	if route.skipRewrite() {
		p.recordSemanticRoute(semanticRouteSkip, latency)
	} else {
		p.recordSemanticRoute(semanticRouteSelected, latency)
	}
	p.logSemantic(ctx, schemas.LogLevelInfo,
		"2/route: vllm-sr response took=%s selected=%s recommended=%v decision=%q algorithm=%q status=%q method=%q",
		formatTook(latency), route.SelectedModel, route.Candidates, route.Decision, route.Algorithm, route.SelectionStatus, route.SelectionMethod)
	return route, nil
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline")
}

func buildVLLMSRPreviewRequest(body map[string]any, profile *RequestProfile, pool []routeCandidate, cfg *VLLMSRRouterConfig) map[string]any {
	// vLLM-SR's preview endpoint is OpenAI-chat shaped and rejects unknown
	// fields. In particular, eligible_models, candidates, request_type, and
	// request_profile return 400 INVALID_INPUT. VK-pool intersection and hard
	// capability validation happen locally after the preview response.
	_ = profile
	_ = pool
	req := map[string]any{
		"model":    cfg.entrypoint(),
		"messages": extractChatMessages(body),
	}

	// Forward only fields declared by the preview API contract.
	for _, key := range []string{
		"function_call",
		"functions",
		"max_completion_tokens",
		"max_tokens",
		"metadata",
		"options",
		"preview_context",
		"response_format",
		"text",
		"tool_choice",
		"tools",
	} {
		if value, ok := body[key]; ok {
			req[key] = value
		}
	}
	return req
}

func extractChatMessages(body map[string]any) []map[string]any {
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		out := make([]map[string]any, 0, len(messages))
		for _, item := range messages {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := message["role"].(string)
			if role == "" {
				role = "user"
			}
			content := previewContentForRouter(message["content"])
			if content == nil {
				continue
			}
			out = append(out, map[string]any{
				"role":    role,
				"content": content,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	if text := extractPreviewUserText(body); text != "" {
		return []map[string]any{{"role": "user", "content": truncateRunes(text, vllmsrMessagesMaxChars)}}
	}
	return []map[string]any{{"role": "user", "content": ""}}
}

// previewContentForRouter keeps non-text parts visible to the router so its
// input_modality signals can fire, while never shipping inline media bytes.
// Text-only content collapses back to a plain string. Returns nil when the
// message carries nothing the router can use.
func previewContentForRouter(content any) any {
	parts, ok := content.([]any)
	if !ok {
		text := textFromContent(content)
		if text == "" {
			return nil
		}
		return truncateRunes(text, vllmsrMessagesMaxChars)
	}

	out := make([]any, 0, len(parts))
	textOnly := true
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, map[string]any{"type": "text", "text": truncateRunes(text, vllmsrMessagesMaxChars)})
			}
			continue
		}
		kind, _ := part["type"].(string)
		switch kind {
		case "image_url", "input_image", "input_audio", "audio", "video_url", "input_video", "file", "input_file":
			textOnly = false
			out = append(out, mediaPartForRouter(kind, part))
		default:
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, map[string]any{"type": "text", "text": truncateRunes(text, vllmsrMessagesMaxChars)})
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	if textOnly {
		text := textFromContent(content)
		if text == "" {
			return nil
		}
		return truncateRunes(text, vllmsrMessagesMaxChars)
	}
	return out
}

// mediaPartForRouter rebuilds a media part with its payload replaced by a short
// reference. Base64 data URLs can be megabytes; the router only needs the kind.
func mediaPartForRouter(kind string, part map[string]any) map[string]any {
	rebuilt := map[string]any{"type": kind}
	switch nested := part[kind].(type) {
	case map[string]any:
		inner := map[string]any{}
		if url, ok := nested["url"].(string); ok {
			inner["url"] = mediaReference(url)
		}
		if detail, ok := nested["detail"].(string); ok {
			inner["detail"] = detail
		}
		rebuilt[kind] = inner
	case string:
		rebuilt[kind] = mediaReference(nested)
	}
	return rebuilt
}

func mediaReference(url string) string {
	if strings.HasPrefix(strings.TrimSpace(url), "data:") {
		return "data:inline"
	}
	return truncateRunes(url, vllmsrMediaRefMaxChars)
}

func parseVLLMSRPreview(body []byte) (*vllmsrRoute, error) {
	var raw map[string]any
	if err := sonic.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode vllm-sr preview: %w", err)
	}

	route := &vllmsrRoute{
		SelectedModel:   firstString(raw, "selected_model", "selectedModel"),
		Decision:        firstString(raw, "selected_decision", "selectedDecision", "decision", "routing_decision", "routingDecision"),
		Algorithm:       firstString(raw, "algorithm", "selection_method", "selectionMethod"),
		SelectionStatus: firstString(raw, "selection_status", "selectionStatus"),
		SelectionMethod: firstString(raw, "selection_method", "selectionMethod"),
		Candidates:      collectModelIDs(raw["recommended_models"], raw["recommendedModels"], raw["candidates"], raw["ranked_models"], raw["rankedModels"], raw["model_refs"], raw["modelRefs"]),
	}

	if nested, ok := raw["decision_result"].(map[string]any); ok {
		if route.Decision == "" {
			route.Decision = firstString(nested, "decision_name", "decisionName", "name", "id")
		}
		if route.Algorithm == "" {
			route.Algorithm = firstString(nested, "algorithm")
		}
	}
	if nested, ok := raw["decision"].(map[string]any); ok {
		if route.Decision == "" {
			route.Decision = firstString(nested, "name", "id")
		}
	}
	if nested, ok := raw["algorithm"].(map[string]any); ok {
		if route.Algorithm == "" || strings.Contains(route.Algorithm, "{") {
			route.Algorithm = firstString(nested, "type", "name")
		}
	}
	if route.SelectedModel == "" && len(route.Candidates) > 0 {
		route.SelectedModel = route.Candidates[0]
	}
	if route.SelectedModel == "" && !route.skipRewrite() {
		return nil, fmt.Errorf("vllm-sr preview missing selected_model")
	}
	return route, nil
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		switch v := raw[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func stringMap(value any) map[string]string {
	raw, ok := value.(map[string]any)
	if !ok {
		if typed, typedOK := value.(map[string]string); typedOK {
			return typed
		}
		return nil
	}
	result := make(map[string]string, len(raw))
	for key, value := range raw {
		if text, ok := value.(string); ok {
			result[key] = text
		}
	}
	return result
}

func anyMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func collectModelIDs(values ...any) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch item := v.(type) {
		case nil:
			return
		case string:
			id := strings.TrimSpace(item)
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			out = append(out, id)
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			if id := firstString(item, "model", "name", "id", "selected_model"); id != "" {
				walk(id)
			}
		}
	}
	for _, v := range values {
		walk(v)
	}
	return out
}

// intersectRouteWithEligible orders L1 survivors by the vLLM-SR route.
// Models not in L1 are dropped. Remaining L1 models keep their original order as tail fallbacks.
func intersectRouteWithEligible(eligible []routeCandidate, route *vllmsrRoute) []routeCandidate {
	if len(eligible) == 0 || route == nil {
		return nil
	}

	index := make(map[string]routeCandidate, len(eligible)*2)
	order := make([]string, 0, len(eligible))
	for _, candidate := range eligible {
		qualified := candidate.qualified()
		index[strings.ToLower(qualified)] = candidate
		index[strings.ToLower(candidate.Model)] = candidate
		order = append(order, qualified)
	}

	preferred := make([]string, 0, 1+len(route.Candidates))
	if route.SelectedModel != "" {
		preferred = append(preferred, route.SelectedModel)
	}
	preferred = append(preferred, route.Candidates...)

	used := map[string]bool{}
	out := make([]routeCandidate, 0, len(eligible))
	for _, id := range preferred {
		candidate, ok := lookupEligible(index, id)
		if !ok {
			continue
		}
		key := candidate.qualified()
		if used[key] {
			continue
		}
		used[key] = true
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return nil
	}
	for _, qualified := range order {
		if used[qualified] {
			continue
		}
		out = append(out, index[strings.ToLower(qualified)])
	}
	return out
}

func lookupEligible(index map[string]routeCandidate, id string) (routeCandidate, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return routeCandidate{}, false
	}
	if candidate, ok := index[strings.ToLower(id)]; ok {
		return candidate, true
	}
	if _, model := schemas.ParseModelString(id, ""); model != "" {
		if candidate, ok := index[strings.ToLower(model)]; ok {
			return candidate, true
		}
	}
	return routeCandidate{}, false
}

// extractPreviewUserText returns the last user turn (or prompt/input) for the
// preview messages fallback when the body has no structured messages.
func extractPreviewUserText(body map[string]any) string {
	const maxChars = 2000
	messages, ok := body["messages"].([]any)
	if ok {
		for i := len(messages) - 1; i >= 0; i-- {
			message, ok := messages[i].(map[string]any)
			if !ok {
				continue
			}
			if role, ok := message["role"].(string); ok && role != "user" {
				continue
			}
			if text := textFromContent(message["content"]); text != "" {
				return truncateRunes(text, maxChars)
			}
		}
	}
	for _, field := range []string{"prompt", "input"} {
		if text := textFromContent(body[field]); text != "" {
			return truncateRunes(text, maxChars)
		}
	}
	return ""
}

func textFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			switch part := item.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
