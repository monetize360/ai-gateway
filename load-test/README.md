# Bifrost tenant-store load test

End-to-end load and governance tests against a **local Bifrost** started with the repo-root [`config.json`](../config.json) (`tenant_store.enabled=true`).

Bifrost reads the first row from the MPilot **global** `tenants` table, opens that tenant's Postgres database, and validates inference requests with the MPilot **virtual-key JWT** (RS256).

Docker runs only **fake-llm** (and optionally Kafka for legacy flush metrics). Bifrost itself runs via `./start-dev.sh` on the host.

## Architecture

```
seed_tenant.py ──► MPilot global DB (tenants)
        │
        └──► tenant DB (config_providers, routing_rules, budgets, rate limits, virtual keys)

governance_test.py / load_test.py
        │
        ▼  Authorization: Bearer <virtual-key JWT>
Bifrost (:8091 default, tenant_store)
        │
        ▼
fake-llm (:18000 default)
```

## Prerequisites

- MPilot Postgres on `localhost:5432` with global DB `mpilotv2` and at least one tenant row in `tenants`
- FinOps module migrated on that tenant (tables: `config_providers`, `routing_rules`, `governance_*`)
- Python 3.10+ (`pip install -r scripts/requirements.txt`)

## Quick start

**Terminal 1** (optional — `run.sh` starts Bifrost if it is not already up):

```bash
cd /path/to/ai-gateway
APP_DIR="$(pwd)" ./start-dev.sh
```

**Terminal 2** — fake-llm, seed tenant, governance tests, load test:

```bash
cd load-test
./scripts/run.sh
```

`run.sh` will:

1. Start **fake-llm** on `:18000` (override with `FAKE_LLM_PORT`)
2. **Seed** the first global tenant (schema patches + fakellm provider URL) **before** Bifrost starts
3. Start Bifrost on `:8091` with `APP_DIR=<repo-root>` when `/health` is not already up
4. Run **governance tests** (routing → rate limit → budget)
5. Run the **load test** with `--tenant-auth` and model `fakellm-openai/gpt-4o-mini`

Environment overrides:

```bash
BIFROST_PORT=8091 FAKE_LLM_PORT=18000 ./scripts/run.sh
```

### Governance tests

| Test | Setup | Expected |
|------|--------|----------|
| Routing | SQL toggle `routing_rules.enabled` + wait for tenant sync (`updated_at` bump) | Without rule: failure; with rule: `fakellm-openai/gpt-4o-mini` |
| Rate limit | `PUT /api/governance/virtual-keys/{id}` with `request_max_limit=1`, then 2 inference calls | 1st **200**, 2nd **429** |
| Budget | `PUT` tight budget + SQL pre-exhaust `current_usage` + `PUT` reload | HTTP **402** |

**Notes**

- Routing can be driven via SQL because routing rules refresh from DB without the dump-then-refresh race that affects budgets/rate limits.
- Rate limit / budget updates use the **governance HTTP API** so in-memory state reloads immediately (`ReloadVirtualKey`). Raw SQL mid-test is overwritten on the 10s tenant sync.
- MPilot tenant JWTs must include a real `users.id` for `updated_by` FK on governance writes.
- `seed_tenant.py` applies idempotent schema patches (Bifrost columns missing on older tenant finops tables).
- Rebuild/restart Bifrost after seed schema patches to avoid Postgres cached-plan errors.

Run governance tests only:

```bash
python3 scripts/seed_tenant.py
python3 scripts/governance_test.py --bifrost-url http://localhost:8080
```

### Load test only

```bash
python3 scripts/load_test.py \
  --total 5000 \
  --concurrency 50 \
  --tenant-auth \
  --model fakellm-openai/gpt-4o-mini \
  --config-json ../config.json
```

## Environment variables (`run.sh`)

| Variable | Default | Description |
|----------|---------|-------------|
| `BIFROST_URL` | `http://localhost:8080` | Bifrost base URL |
| `FAKE_LLM_URL` | `http://localhost:8000/` | Upstream for `fakellm-openai` |
| `TOTAL_REQUESTS` | `5000` | Load test request count |
| `CONCURRENCY` | `50` | Load test concurrency |
| `RUN_GOVERNANCE_TEST` | `1` | Set `0` to skip governance tests |
| `RUN_LOAD_TEST` | `1` | Set `0` to skip throughput test |
| `START_KAFKA` | `0` | Set `1` to also start Kafka + flush metrics |
| `JWT_VIRTUAL_KEY_PRIVATE` | *(from mpilotv2 `application.properties`)* | Override JWT signing key for tests |

## Manual smoke test

```bash
# After seed_tenant.py — mint token via governance_test helpers or MPilot UI virtual-key token API
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"fakellm-openai/gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'
```

## Stop services

```bash
cd load-test
docker compose down
# Ctrl+C the terminal running start-dev.sh
```

## Legacy dev-config mode

The old single-Postgres `load-test/dev-config/config.json` path (embedded `fake-llm` provider, no tenant JWT) is deprecated. Use repo-root `config.json` + `scripts/seed_tenant.py` instead.
