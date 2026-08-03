package kafkainject

import "time"

// Standard billing-seeded Kafka connection / datasource IDs.
const (
	DefaultConnectionID = "b2c3d4e5-f6a7-4890-b123-456789abcdef"
	DefaultDataSourceID = "c3d4e5f6-a7b8-4901-c234-56789abcdef0"
)

const (
	ProducerCacheTTL = 5 * time.Minute
	SchemaCacheTTL   = 10 * time.Minute
	JWTCacheTTL      = 30 * time.Second
	ProduceTimeout   = 10 * time.Second
	MaxMessageBytes  = 1 << 20 // 1 MiB
)
