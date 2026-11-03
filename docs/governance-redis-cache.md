# MPilot Governance Redis Cache

MPilot owns Gateway governance configuration. It writes changed governance
entities to the shared Redis namespace and, only after its database transaction
commits, publishes a tenant version event on
`mpilot:ai-gateway:governance:changed`.

Gateway lookup order is:

1. bounded, per-process LRU snapshot;
2. Redis entity hashes for the tenant;
3. tenant Postgres only when a tenant has no Redis namespace yet.

A notification refreshes the tenant's warm in-memory store from Redis. Every
`tenant_store.refresh_interval_seconds` (default 10s) the Gateway also compares
each warm tenant's applied version with Redis in one `MGET` and refreshes any
tenant that is behind, so a notification lost during a reconnect or failover is
applied within one interval. A snapshot whose version is already applied is not
rebuilt.

Redis does not carry user org-unit mappings, org/org-unit/user allowed-model
configs and provider access, or billing alert thresholds. Those are read from
the tenant database whenever a new Redis version is applied.

Rate-limit counters are owned by each Gateway process and carried across Redis
refreshes; Redis values never overwrite them.

Budget spend (`BudgetUsage.current_usage`) is written to Postgres by MPilot
Rating. Every `tenant_store.refresh_interval_seconds` the Gateway reads back
only the budget usage rows changed since its last read (one small query per
warm tenant), so budgets enforce whether or not MPilot bumps the Redis version
after rating. When a Redis version is applied, spend is taken from Postgres,
not from the Redis copy.

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
