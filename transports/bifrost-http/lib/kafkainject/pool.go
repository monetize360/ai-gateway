package kafkainject

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
	kgo "github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// ProducerEntry is a cached Kafka producer bound to a topic and mObject.
type ProducerEntry struct {
	Client    *kgo.Client
	Topic     string
	MObjectID string
	LoadedAt  time.Time
}

// Pool caches franz-go producers per tenant + connection + datasource.
type Pool struct {
	registry tenantstore.Resolver
	mu       sync.RWMutex
	entries  map[string]*ProducerEntry
}

// NewPool creates an empty producer pool.
func NewPool(registry tenantstore.Resolver) *Pool {
	return &Pool{
		registry: registry,
		entries:  make(map[string]*ProducerEntry),
	}
}

func poolKey(tenantID, connectionID, dataSourceID string) string {
	return tenantID + ":" + connectionID + ":" + dataSourceID
}

func standardPoolKey(tenantID, bootstrapServers, topic string) string {
	return "standard:" + tenantID + ":" + bootstrapServers + ":" + topic
}

// GetOrCreate returns a cached producer or loads connection/datasource from the tenant DB.
func (p *Pool) GetOrCreate(ctx context.Context, tenantID, connectionID, dataSourceID string) (*ProducerEntry, error) {
	key := poolKey(tenantID, connectionID, dataSourceID)

	p.mu.RLock()
	if e, ok := p.entries[key]; ok && time.Since(e.LoadedAt) < ProducerCacheTTL {
		p.mu.RUnlock()
		return e, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok && time.Since(e.LoadedAt) < ProducerCacheTTL {
		return e, nil
	}
	if old, ok := p.entries[key]; ok {
		delete(p.entries, key)
		go old.Client.Close()
	}

	entry, err := p.load(ctx, tenantID, connectionID, dataSourceID)
	if err != nil {
		return nil, err
	}
	p.entries[key] = entry
	return entry, nil
}

// GetOrCreateStandard returns the environment-configured producer for a tenant.
// This path intentionally does not read integration connections or datasources.
func (p *Pool) GetOrCreateStandard(tenantID string) (*ProducerEntry, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, fmt.Errorf("tenantID is required")
	}

	bootstrapServers := strings.TrimSpace(os.Getenv(KafkaBootstrapServersEnv))
	if bootstrapServers == "" {
		return nil, fmt.Errorf("%s is not configured", KafkaBootstrapServersEnv)
	}
	topicPrefix := strings.TrimSpace(os.Getenv(KafkaUsageTopicPrefixEnv))
	if topicPrefix == "" {
		topicPrefix = DefaultUsageTopicPrefix
	}
	// Keep this derivation identical to MPilot UsageKafkaTopics.topic:
	// KAFKA_USAGE_TOPIC_PREFIX + "_" + tenantId.
	topic := topicPrefix + "_" + tenantID
	key := standardPoolKey(tenantID, bootstrapServers, topic)

	p.mu.RLock()
	if entry, ok := p.entries[key]; ok && time.Since(entry.LoadedAt) < ProducerCacheTTL {
		p.mu.RUnlock()
		return entry, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.entries[key]; ok && time.Since(entry.LoadedAt) < ProducerCacheTTL {
		return entry, nil
	}
	if old, ok := p.entries[key]; ok {
		delete(p.entries, key)
		go old.Client.Close()
	}

	clientID := strings.TrimSpace(os.Getenv(KafkaClientIDEnv))
	if clientID == "" {
		clientID = DefaultKafkaClientID
	}
	client, err := newKafkaClient(&kafkaConnectionConfig{
		BootstrapServers: bootstrapServers,
		ClientID:         clientID,
	})
	if err != nil {
		return nil, err
	}
	entry := &ProducerEntry{
		Client:   client,
		Topic:    topic,
		LoadedAt: time.Now(),
	}
	p.entries[key] = entry
	return entry, nil
}

// Close closes all cached producers.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		e.Client.Close()
		delete(p.entries, k)
	}
}

type connectionRow struct {
	Configuration json.RawMessage `gorm:"column:configuration"`
}

type dataSourceRow struct {
	MObjectID           string          `gorm:"column:mobject_id"`
	SourceConfiguration json.RawMessage `gorm:"column:source_configuration"`
}

type kafkaConnectionConfig struct {
	Type               string               `json:"type"`
	BootstrapServers   string               `json:"bootstrapServers"`
	ClientID           string               `json:"clientId"`
	AuthenticationType string               `json:"authenticationType"`
	ProducerConfig     *kafkaProducerConfig `json:"producerConfig"`
	SASLDetails        *struct {
		Username  string `json:"username"`
		Password  any    `json:"password"`
		Mechanism string `json:"mechanism"`
	} `json:"saslDetails"`
}

type kafkaDataSourceConfig struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
}

func (p *Pool) load(ctx context.Context, tenantID, connectionID, dataSourceID string) (*ProducerEntry, error) {
	if p.registry == nil {
		return nil, fmt.Errorf("tenant registry not configured")
	}
	store := p.registry.GetStoreForTenant(ctx, tenantID)
	if store == nil {
		return nil, fmt.Errorf("unknown tenant")
	}

	conn, ds, err := loadConnectionAndDataSource(ctx, store, connectionID, dataSourceID)
	if err != nil {
		return nil, err
	}

	var conf kafkaConnectionConfig
	if err := json.Unmarshal(conn.Configuration, &conf); err != nil {
		return nil, fmt.Errorf("invalid kafka connection configuration: %w", err)
	}
	var dsConf kafkaDataSourceConfig
	if err := json.Unmarshal(ds.SourceConfiguration, &dsConf); err != nil {
		return nil, fmt.Errorf("invalid kafka datasource configuration: %w", err)
	}
	if strings.TrimSpace(dsConf.Topic) == "" {
		return nil, fmt.Errorf("datasource topic is missing")
	}
	if strings.TrimSpace(conf.BootstrapServers) == "" {
		return nil, fmt.Errorf("connection bootstrapServers is missing")
	}

	client, err := newKafkaClient(&conf)
	if err != nil {
		return nil, err
	}
	return &ProducerEntry{
		Client:    client,
		Topic:     dsConf.Topic,
		MObjectID: ds.MObjectID,
		LoadedAt:  time.Now(),
	}, nil
}

func loadConnectionAndDataSource(ctx context.Context, store configstore.ConfigStore, connectionID, dataSourceID string) (*connectionRow, *dataSourceRow, error) {
	db := store.DB().WithContext(ctx)

	var conn connectionRow
	err := db.Raw(`
		SELECT configuration
		FROM m_integration_connection
		WHERE id = ?::uuid
		  AND integration_type = 'KAFKA'
		  AND COALESCE(deleted, false) = false
		LIMIT 1
	`, connectionID).Scan(&conn).Error
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load kafka connection: %w", err)
	}
	if len(conn.Configuration) == 0 {
		return nil, nil, fmt.Errorf("kafka connection not found: %s", connectionID)
	}

	var ds dataSourceRow
	err = db.Raw(`
		SELECT mobject_id::text AS mobject_id, source_configuration
		FROM m_integration_datasource
		WHERE id = ?::uuid
		  AND connection_id = ?::uuid
		  AND COALESCE(deleted, false) = false
		LIMIT 1
	`, dataSourceID, connectionID).Scan(&ds).Error
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load kafka datasource: %w", err)
	}
	if ds.MObjectID == "" || len(ds.SourceConfiguration) == 0 {
		return nil, nil, fmt.Errorf("kafka datasource not found: %s", dataSourceID)
	}
	return &conn, &ds, nil
}

func newKafkaClient(conf *kafkaConnectionConfig) (*kgo.Client, error) {
	brokers := splitBrokers(conf.BootstrapServers)
	if len(brokers) == 0 {
		return nil, fmt.Errorf("no bootstrap brokers configured")
	}
	clientID := conf.ClientID
	if clientID == "" {
		clientID = DefaultKafkaClientID
	}

	settings := resolveProducerSettings(conf.ProducerConfig)
	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.RequiredAcks(settings.RequiredAcks),
		kgo.ProducerLinger(lingerDuration(settings.LingerMs)),
		kgo.ProducerBatchMaxBytes(ProducerBatchMaxBytes),
		kgo.MaxBufferedRecords(MaxBufferedRecords),
		kgo.RecordPartitioner(kgo.RoundRobinPartitioner()),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()),
	}
	// franz-go enables idempotent writes by default, which only works with acks=all.
	if settings.AcksLabel != "all" {
		opts = append(opts, kgo.DisableIdempotentWrite())
	}

	auth := strings.ToUpper(strings.TrimSpace(conf.AuthenticationType))
	switch auth {
	case "", "PLAINTEXT":
		// no extra security
	case "SASL":
		if conf.SASLDetails == nil {
			return nil, fmt.Errorf("SASL authentication configured but saslDetails missing")
		}
		password := extractSecretText(conf.SASLDetails.Password)
		username := conf.SASLDetails.Username
		mechanism := strings.ToUpper(strings.TrimSpace(conf.SASLDetails.Mechanism))
		switch mechanism {
		case "", "PLAIN", "SASL_PLAIN":
			opts = append(opts, kgo.SASL(plain.Auth{User: username, Pass: password}.AsMechanism()))
		case "SCRAM-SHA-256", "SCRAM_SHA_256", "SCRAMSHA256":
			opts = append(opts, kgo.SASL(scram.Auth{User: username, Pass: password}.AsSha256Mechanism()))
		case "SCRAM-SHA-512", "SCRAM_SHA_512", "SCRAMSHA512":
			opts = append(opts, kgo.SASL(scram.Auth{User: username, Pass: password}.AsSha512Mechanism()))
		default:
			return nil, fmt.Errorf("unsupported SASL mechanism: %s", conf.SASLDetails.Mechanism)
		}
	default:
		return nil, fmt.Errorf("unsupported kafka authenticationType: %s", conf.AuthenticationType)
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create kafka client: %w", err)
	}
	return client, nil
}

func splitBrokers(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func extractSecretText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		if text, ok := t["text"].(string); ok {
			return text
		}
		if text, ok := t["value"].(string); ok {
			return text
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var wrapper struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &wrapper) == nil {
		return wrapper.Text
	}
	return ""
}

// ProduceSync publishes a record and waits for broker acknowledgment.
// Uses async Produce + callback so HTTP goroutines can pipeline in-flight records.
func ProduceSync(ctx context.Context, entry *ProducerEntry, key string, value []byte) (topic string, partition int32, offset int64, err error) {
	if entry == nil || entry.Client == nil {
		return "", 0, 0, fmt.Errorf("kafka producer not available")
	}
	payload := append([]byte(nil), value...)
	rec := &kgo.Record{
		Topic: entry.Topic,
		Value: payload,
	}
	if key != "" {
		rec.Key = []byte(key)
	}

	produceCtx, cancel := context.WithTimeout(ctx, ProduceTimeout)
	defer cancel()

	type produceResult struct {
		topic     string
		partition int32
		offset    int64
		err       error
	}
	done := make(chan produceResult, 1)
	entry.Client.Produce(produceCtx, rec, func(r *kgo.Record, produceErr error) {
		res := produceResult{err: produceErr}
		if r != nil {
			res.topic = r.Topic
			res.partition = r.Partition
			res.offset = r.Offset
		}
		done <- res
	})

	select {
	case res := <-done:
		if res.err != nil {
			return "", 0, 0, res.err
		}
		if res.topic == "" {
			res.topic = entry.Topic
		}
		return res.topic, res.partition, res.offset, nil
	case <-produceCtx.Done():
		return "", 0, 0, produceCtx.Err()
	}
}
