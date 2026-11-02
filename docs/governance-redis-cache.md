# MPilot Governance Redis Cache

MPilot owns Gateway governance configuration. It writes changed governance
entities to the shared Redis namespace and, only after its database transaction
commits, publishes a tenant version event on
`mpilot:ai-gateway:governance:changed`.

Gateway lookup order is:

1. bounded, per-process LRU snapshot;
2. Redis entity hashes for the tenant;
3. tenant Postgres only when a tenant has no Redis namespace yet.

The Gateway checks the Redis tenant version when it uses an LRU snapshot, so a
missed pub/sub notification is recovered on the next request. A notification
evicts the tenant's local components; the next request hydrates the rebuilt
snapshot from Redis.

Configuration lives at `tenant_store.governance_cache`:

```json
{
  "enabled": true,
  "address": "${AI_GATEWAY_REDIS_ADDRESS}",
  "password": "${AI_GATEWAY_REDIS_PASSWORD}",
  "tls": true,
  "key_prefix": "mpilot:ai-gateway:governance",
  "channel": "mpilot:ai-gateway:governance:changed",
  "lru_capacity": 2000,
  "lru_ttl_seconds": 300
}
```

Use a Redis ACL scoped to the configured key prefix and channel. Redis data can
contain provider and virtual-key configuration; do not expose it through logs,
metrics, or general-purpose Redis clients.

The old fixed governance database refresh is disabled when this cache is
enabled. The rate-limit dump/reset worker remains active because it persists
locally accumulated request counters.
