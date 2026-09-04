package kafkainject

import "time"

// Standard billing-seeded Kafka connection / datasource IDs.
const (
	DefaultConnectionID = "b2c3d4e5-f6a7-4890-b123-456789abcdef"
	DefaultDataSourceID = "c3d4e5f6-a7b8-4901-c234-56789abcdef0"

	KafkaBootstrapServersEnv = "KAFKA_BOOTSTRAP_SERVERS"
	KafkaUsageTopicPrefixEnv = "KAFKA_USAGE_TOPIC_PREFIX"
	KafkaClientIDEnv         = "KAFKA_CLIENT_ID"

	// DefaultUsageTopicPrefix must match AppKafkaProperties.Usage.topicPrefix
	// in MPilot so producers and consumers resolve the same per-tenant topic.
	DefaultUsageTopicPrefix = "inference-usage"
	DefaultKafkaClientID    = "ai-gateway-kafka-ingest"
)

const (
	ProducerCacheTTL = 5 * time.Minute
	SchemaCacheTTL   = 10 * time.Minute
	JWTCacheTTL      = 30 * time.Second
	ProduceTimeout   = 10 * time.Second
	MaxMessageBytes  = 1 << 20 // 1 MiB

	// DefaultProducerLingerMs is used only when lingerMs is omitted (Kafka client default).
	DefaultProducerLingerMs = 5
	ProducerBatchMaxBytes   = 1 << 20
	MaxBufferedRecords      = 500_000
	// DefaultProducerAcks is ISR acks=all. Connection JSON can override (e.g. "1").
	DefaultProducerAcks = "all"
)
