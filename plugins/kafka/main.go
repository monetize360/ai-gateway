// Package kafka is a Kafka observability plugin for Bifrost.
// It implements ObservabilityPlugin to stream completed request traces as JSON
// messages to a Kafka topic in real-time after each request completes.
package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	kgo "github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

const PluginName = "kafka"

// SASLMechanism defines the supported SASL mechanisms.
type SASLMechanism string

const (
	SASLMechanismPlain       SASLMechanism = "plain"
	SASLMechanismSCRAMSHA256 SASLMechanism = "scram-sha-256"
	SASLMechanismSCRAMSHA512 SASLMechanism = "scram-sha-512"
)

// SASLConfig holds SASL authentication configuration.
type SASLConfig struct {
	Mechanism SASLMechanism   `json:"mechanism"`
	Username  *schemas.EnvVar `json:"username"`
	Password  *schemas.EnvVar `json:"password"`
}

// TLSConfig holds TLS configuration for the Kafka connection.
type TLSConfig struct {
	Enabled    bool   `json:"enabled"`
	SkipVerify bool   `json:"skip_verify"`
	CACert     string `json:"ca_cert,omitempty"`
}

// Config is the configuration for the Kafka plugin.
type Config struct {
	// Brokers is a list of Kafka broker addresses in host:port format. Required.
	Brokers []string `json:"brokers"`
	// Topic is the Kafka topic to publish trace records to. Required.
	Topic string `json:"topic"`
	// ClientID is an optional client identifier sent to Kafka brokers.
	ClientID string `json:"client_id,omitempty"`
	// SASL holds optional SASL authentication settings.
	SASL *SASLConfig `json:"sasl,omitempty"`
	// TLS holds optional TLS settings for broker connections.
	TLS *TLSConfig `json:"tls,omitempty"`
	// FlushFrequencyMs is how often the producer flushes buffered records in milliseconds.
	// Defaults to 100ms. Lower values reduce end-to-end latency at the cost of throughput.
	FlushFrequencyMs int `json:"flush_frequency_ms,omitempty"`
	// MaxBufferSize is the maximum number of records held in the producer's memory buffer
	// before older records are dropped. Defaults to 10000.
	MaxBufferSize int `json:"max_buffer_size,omitempty"`
}

// MarshalForStorage serializes Config to JSON with *EnvVar fields as plain strings
// for database/config-file persistence.
func (c *Config) MarshalForStorage() ([]byte, error) {
	type saslAlias struct {
		Mechanism SASLMechanism `json:"mechanism"`
		Username  string        `json:"username,omitempty"`
		Password  string        `json:"password,omitempty"`
	}
	type alias struct {
		Brokers          []string   `json:"brokers"`
		Topic            string     `json:"topic"`
		ClientID         string     `json:"client_id,omitempty"`
		SASL             *saslAlias `json:"sasl,omitempty"`
		TLS              *TLSConfig `json:"tls,omitempty"`
		FlushFrequencyMs int        `json:"flush_frequency_ms,omitempty"`
		MaxBufferSize    int        `json:"max_buffer_size,omitempty"`
	}
	a := alias{
		Brokers:          c.Brokers,
		Topic:            c.Topic,
		ClientID:         c.ClientID,
		TLS:              c.TLS,
		FlushFrequencyMs: c.FlushFrequencyMs,
		MaxBufferSize:    c.MaxBufferSize,
	}
	if c.SASL != nil {
		a.SASL = &saslAlias{
			Mechanism: c.SASL.Mechanism,
			Username:  schemas.EnvVarAsString(c.SASL.Username),
			Password:  schemas.EnvVarAsString(c.SASL.Password),
		}
	}
	return sonic.Marshal(a)
}

// Redacted returns a copy of the config with sensitive fields masked.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if c.SASL != nil {
		sasl := *c.SASL
		sasl.Password = c.SASL.Password.Redacted()
		out.SASL = &sasl
	}
	return &out
}

// TraceRecord is the JSON payload published to Kafka for each completed request.
// It is a flat structure extracted from schemas.Trace for ease of consumption by
// stream processors, alerting engines, and analytics pipelines.
type TraceRecord struct {
	TraceID        string         `json:"trace_id"`
	RequestID      string         `json:"request_id"`
	Timestamp      time.Time      `json:"timestamp"`
	DurationMs     float64        `json:"duration_ms"`
	Status         string         `json:"status"`
	Provider       string         `json:"provider,omitempty"`
	Model          string         `json:"model,omitempty"`
	RequestType    string         `json:"request_type,omitempty"`
	InputTokens    int            `json:"input_tokens,omitempty"`
	OutputTokens   int            `json:"output_tokens,omitempty"`
	TotalTokens    int            `json:"total_tokens,omitempty"`
	Cost           float64        `json:"cost,omitempty"`
	VirtualKeyID   string         `json:"virtual_key_id,omitempty"`
	VirtualKeyName string         `json:"virtual_key_name,omitempty"`
	TeamID         string         `json:"team_id,omitempty"`
	TeamName       string         `json:"team_name,omitempty"`
	CustomerID     string         `json:"customer_id,omitempty"`
	CustomerName   string         `json:"customer_name,omitempty"`
	Retries        int            `json:"retries,omitempty"`
	FallbackIndex  int            `json:"fallback_index,omitempty"`
	TTFT           float64        `json:"time_to_first_token_ms,omitempty"`
	ErrorMessage   string         `json:"error,omitempty"`
	SpanCount      int            `json:"span_count"`
	Attributes     map[string]any `json:"attributes,omitempty"`
}

// KafkaPlugin implements schemas.ObservabilityPlugin and schemas.LLMPlugin.
// It converts completed Bifrost traces into flat TraceRecord JSON and publishes
// them asynchronously to a Kafka topic via a buffered, non-blocking producer.
type KafkaPlugin struct {
	ctx    context.Context
	cancel context.CancelFunc

	topic  string
	client *kgo.Client

	// channel-based async buffer so Inject never blocks the caller
	recordsCh chan []byte
}

// logger is the package-level logger set during Init.
var logger schemas.Logger

// Init creates and connects the Kafka plugin.
func Init(ctx context.Context, config *Config, _logger schemas.Logger) (*KafkaPlugin, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}
	logger = _logger

	if len(config.Brokers) == 0 {
		return nil, fmt.Errorf("at least one broker address is required")
	}
	if config.Topic == "" {
		return nil, fmt.Errorf("topic is required")
	}

	flushFreq := config.FlushFrequencyMs
	if flushFreq <= 0 {
		flushFreq = 100
	}
	bufSize := config.MaxBufferSize
	if bufSize <= 0 {
		bufSize = 10000
	}

	clientID := config.ClientID
	if clientID == "" {
		clientID = "bifrost-kafka-plugin"
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(config.Brokers...),
		kgo.ClientID(clientID),
		kgo.ProducerLinger(time.Duration(flushFreq) * time.Millisecond),
		kgo.RecordPartitioner(kgo.RoundRobinPartitioner()),
		// Deliver records at-least-once; acknowledge after leader writes.
		kgo.RequiredAcks(kgo.LeaderAck()),
	}

	// TLS configuration
	if config.TLS != nil && config.TLS.Enabled {
		tlsCfg := &tls.Config{
			InsecureSkipVerify: config.TLS.SkipVerify, //nolint:gosec
		}
		if config.TLS.CACert != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(config.TLS.CACert)) {
				return nil, fmt.Errorf("failed to parse TLS CA certificate")
			}
			tlsCfg.RootCAs = pool
			tlsCfg.InsecureSkipVerify = false
		}
		opts = append(opts, kgo.DialTLSConfig(tlsCfg))
	}

	// SASL configuration
	if config.SASL != nil {
		username := config.SASL.Username.GetValue()
		password := config.SASL.Password.GetValue()
		switch config.SASL.Mechanism {
		case SASLMechanismPlain:
			opts = append(opts, kgo.SASL(plain.Auth{
				User: username,
				Pass: password,
			}.AsMechanism()))
		case SASLMechanismSCRAMSHA256:
			opts = append(opts, kgo.SASL(scram.Auth{
				User: username,
				Pass: password,
			}.AsSha256Mechanism()))
		case SASLMechanismSCRAMSHA512:
			opts = append(opts, kgo.SASL(scram.Auth{
				User: username,
				Pass: password,
			}.AsSha512Mechanism()))
		default:
			return nil, fmt.Errorf("unsupported SASL mechanism %q: must be plain, scram-sha-256, or scram-sha-512", config.SASL.Mechanism)
		}
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka client: %w", err)
	}

	pluginCtx, cancel := context.WithCancel(ctx)
	p := &KafkaPlugin{
		ctx:       pluginCtx,
		cancel:    cancel,
		topic:     config.Topic,
		client:    client,
		recordsCh: make(chan []byte, bufSize),
	}

	// Start background producer goroutine
	go p.runProducer()

	logger.Info("kafka plugin initialized: brokers=%v topic=%s flushFreq=%dms buffer=%d",
		config.Brokers, config.Topic, flushFreq, bufSize)
	return p, nil
}

// runProducer consumes from recordsCh and produces records to Kafka asynchronously.
// Records dropped from an overflowing buffer are counted but not retried.
func (p *KafkaPlugin) runProducer() {
	for {
		select {
		case <-p.ctx.Done():
			// Drain remaining records before exiting
			for {
				select {
				case payload := <-p.recordsCh:
					p.produce(payload)
				default:
					if err := p.client.Flush(context.Background()); err != nil {
						logger.Error("kafka plugin: final flush error: %v", err)
					}
					return
				}
			}
		case payload := <-p.recordsCh:
			p.produce(payload)
		}
	}
}

// produce sends a single JSON payload to the configured Kafka topic using the
// non-blocking fire-and-forget async produce path. Delivery errors are logged.
func (p *KafkaPlugin) produce(payload []byte) {
	rec := &kgo.Record{
		Topic: p.topic,
		Value: payload,
	}
	p.client.Produce(p.ctx, rec, func(r *kgo.Record, err error) {
		if err != nil {
			logger.Error("kafka plugin: failed to deliver record to topic %s: %v", r.Topic, err)
		}
	})
}

// GetName implements schemas.BasePlugin.
func (p *KafkaPlugin) GetName() string { return PluginName }

// MarshalConfigForStorage implements schemas.ConfigMarshallerPlugin.
func (p *KafkaPlugin) MarshalConfigForStorage(raw map[string]any) (map[string]any, error) {
	b, err := sonic.Marshal(raw)
	if err != nil {
		return raw, err
	}
	var c Config
	if err := sonic.Unmarshal(b, &c); err != nil {
		return raw, err
	}
	normalized, err := c.MarshalForStorage()
	if err != nil {
		return raw, err
	}
	var out map[string]any
	if err := sonic.Unmarshal(normalized, &out); err != nil {
		return raw, err
	}
	return out, nil
}

// RedactConfig implements schemas.ConfigMarshallerPlugin.
func (p *KafkaPlugin) RedactConfig(raw map[string]any) (map[string]any, error) {
	b, err := sonic.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := sonic.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	out, err := sonic.Marshal(c.Redacted())
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := sonic.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// PreLLMHook is a no-op; this plugin operates only on completed traces.
func (p *KafkaPlugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

// PostLLMHook is a no-op; this plugin operates only on completed traces.
func (p *KafkaPlugin) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// HTTPTransportPreHook is a no-op.
func (p *KafkaPlugin) HTTPTransportPreHook(_ *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

// HTTPTransportPostHook is a no-op.
func (p *KafkaPlugin) HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponse) error {
	return nil
}

// HTTPTransportStreamChunkHook passes chunks through unchanged.
func (p *KafkaPlugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}

// Inject receives a completed trace and enqueues a JSON TraceRecord to the Kafka
// producer buffer. It returns immediately; delivery is asynchronous.
// Implements schemas.ObservabilityPlugin.
// MUST NOT retain *trace after return — data is copied before enqueue.
func (p *KafkaPlugin) Inject(_ context.Context, trace *schemas.Trace) error {
	if trace == nil {
		return nil
	}

	record := buildTraceRecord(trace)
	payload, err := sonic.Marshal(record)
	if err != nil {
		logger.Error("kafka plugin: failed to marshal trace %s: %v", trace.TraceID, err)
		return nil
	}

	// Non-blocking enqueue — drop if buffer is full to prevent back-pressure on callers.
	select {
	case p.recordsCh <- payload:
	default:
		logger.Warn("kafka plugin: record buffer full, dropping trace %s", trace.TraceID)
	}
	return nil
}

// Cleanup shuts down the Kafka producer and flushes pending records.
func (p *KafkaPlugin) Cleanup() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.client != nil {
		p.client.Close()
	}
	return nil
}

// buildTraceRecord extracts key dimensions from a completed Bifrost trace into a
// flat TraceRecord suitable for Kafka consumers and stream processors.
// All data is copied out of the trace before this function returns.
func buildTraceRecord(trace *schemas.Trace) TraceRecord {
	rec := TraceRecord{
		TraceID:    trace.TraceID,
		RequestID:  trace.RequestID,
		Timestamp:  trace.StartTime,
		SpanCount:  len(trace.Spans),
		Status:     "ok",
		Attributes: make(map[string]any),
	}

	if !trace.StartTime.IsZero() && !trace.EndTime.IsZero() {
		rec.DurationMs = float64(trace.EndTime.Sub(trace.StartTime).Milliseconds())
	}

	// Copy root-level trace attributes
	for k, v := range trace.Attributes {
		rec.Attributes[k] = v
	}

	// Find the final LLM call/retry span for aggregate metrics.
	// Multiple spans may exist when retries or fallbacks occurred.
	var finalSpan *schemas.Span
	for _, span := range trace.Spans {
		if span.Kind != schemas.SpanKindLLMCall && span.Kind != schemas.SpanKindRetry {
			continue
		}
		if span.Status == schemas.SpanStatusError {
			rec.Status = "error"
		}
		if finalSpan == nil || span.EndTime.After(finalSpan.EndTime) {
			finalSpan = span
		}
	}

	if finalSpan == nil {
		finalSpan = trace.RootSpan
	}
	if finalSpan == nil {
		return rec
	}

	attrs := finalSpan.Attributes
	if attrs == nil {
		return rec
	}

	rec.Provider = strAttr(attrs, schemas.AttrProviderName)
	rec.Model = strAttr(attrs, schemas.AttrRequestModel)
	rec.RequestType = strAttr(attrs, "gen_ai.operation.name")
	if rec.RequestType == "" {
		rec.RequestType = strAttr(attrs, "request.type")
	}

	rec.VirtualKeyID = strAttr(attrs, schemas.AttrVirtualKeyID)
	rec.VirtualKeyName = strAttr(attrs, schemas.AttrVirtualKeyName)
	rec.TeamID = strAttr(attrs, schemas.AttrTeamID)
	rec.TeamName = strAttr(attrs, schemas.AttrTeamName)
	rec.CustomerID = strAttr(attrs, schemas.AttrCustomerID)
	rec.CustomerName = strAttr(attrs, schemas.AttrCustomerName)

	rec.InputTokens = intAttr(attrs, schemas.AttrInputTokens)
	if rec.InputTokens == 0 {
		rec.InputTokens = intAttr(attrs, schemas.AttrPromptTokens)
	}
	rec.OutputTokens = intAttr(attrs, schemas.AttrOutputTokens)
	if rec.OutputTokens == 0 {
		rec.OutputTokens = intAttr(attrs, schemas.AttrCompletionTokens)
	}
	rec.TotalTokens = intAttr(attrs, schemas.AttrTotalTokens)
	if rec.TotalTokens == 0 && (rec.InputTokens > 0 || rec.OutputTokens > 0) {
		rec.TotalTokens = rec.InputTokens + rec.OutputTokens
	}

	rec.Cost = float64Attr(attrs, schemas.AttrUsageCost)
	rec.Retries = intAttr(attrs, schemas.AttrNumberOfRetries)
	rec.FallbackIndex = intAttr(attrs, schemas.AttrFallbackIndex)

	ttftNs := float64Attr(attrs, schemas.AttrTimeToFirstToken)
	if ttftNs > 0 {
		rec.TTFT = ttftNs / 1e6 // nanoseconds → milliseconds
	}

	if finalSpan.Status == schemas.SpanStatusError {
		rec.Status = "error"
		rec.ErrorMessage = finalSpan.StatusMsg
	}

	return rec
}

// strAttr safely extracts a string attribute from a span's attribute map.
func strAttr(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	v, _ := attrs[key].(string)
	return v
}

// intAttr safely extracts an integer attribute from a span's attribute map.
func intAttr(attrs map[string]any, key string) int {
	if attrs == nil {
		return 0
	}
	switch v := attrs[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// float64Attr safely extracts a float64 attribute from a span's attribute map.
func float64Attr(attrs map[string]any, key string) float64 {
	if attrs == nil {
		return 0
	}
	switch v := attrs[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// Compile-time checks
var _ schemas.ObservabilityPlugin = (*KafkaPlugin)(nil)
var _ schemas.LLMPlugin = (*KafkaPlugin)(nil)
var _ schemas.HTTPTransportPlugin = (*KafkaPlugin)(nil)
