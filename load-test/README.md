# Bifrost + Kafka Load Test (dev server)

End-to-end load test that sends inference traffic through a **local dev Bifrost**
(`./start-dev.sh`) and verifies trace records land in Kafka via the built-in
**Kafka observability connector** plugin.

Docker only runs **Kafka**, **Kafka UI**, and the **fake-llm** upstream — not Bifrost.

## Architecture

```
Load Test (5000 calls)
        │
        ▼ POST /v1/chat/completions
Bifrost dev server (:8080)  — started via ./start-dev.sh
        │ Kafka plugin (ObservabilityPlugin)
        ▼ trace JSON records
Kafka broker (localhost:29092)
        │
        ▼
Kafka UI (:8090)

Bifrost ──► fake-llm (:8000)  (OpenAI-compatible mock upstream)
```

## Services

| Service         | Port   | How it runs                                      |
|-----------------|--------|--------------------------------------------------|
| Bifrost (dev)   | 8080   | `./start-dev.sh` with `load test/dev-config`     |
| Fake LLM API    | 8000   | Docker Compose                                   |
| Kafka (KRaft)   | 29092  | Docker Compose (host listener for dev Bifrost)   |
| Kafka UI        | 8090   | Docker Compose                                   |

Default Kafka topic: `bifrost-traces`

## Quick Start

**Terminal 1** — start dev Bifrost with Kafka connector config pre-loaded:

```bash
cd /path/to/ai-gateway
APP_DIR="$(pwd)/load test/dev-config" ./start-dev.sh
```

**Terminal 2** — start Kafka + fake-llm, configure the connector, run the load test:

```bash
cd "load test"
./scripts/run.sh
```

`run.sh` will:

1. Confirm dev Bifrost is healthy at `http://localhost:8080`
2. Start Kafka and fake-llm via Docker Compose
3. `PUT /api/plugins/kafka` with broker/topic settings (`localhost:29092`)
4. Ensure the `fake-llm` custom provider points at `http://localhost:8000`
5. Fire 5000 concurrent chat completion requests
6. Print latency stats and estimated Kafka message count

### Browse Kafka Messages

Open **http://localhost:8090** → Topics → `bifrost-traces` → Messages.

Each message is a flat JSON `TraceRecord` from the Kafka plugin, for example:

```json
{
  "trace_id": "...",
  "request_id": "...",
  "timestamp": "2026-05-27T12:00:00Z",
  "provider": "fake-llm",
  "model": "gpt-4o-mini",
  "status": "success",
  "latency_ms": 42
}
```

## Manual Smoke Test

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "fake-llm/gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## Custom Parameters

```bash
TOTAL_REQUESTS=1000 CONCURRENCY=20 ./scripts/run.sh

# Or call the Python script directly (Bifrost + Kafka must already be up):
python3 scripts/load_test.py --total 5000 --concurrency 100 --kafka-topic bifrost-traces
```

Environment variables used by `run.sh`:

| Variable         | Default                 |
|------------------|-------------------------|
| `BIFROST_URL`    | `http://localhost:8080` |
| `KAFKA_BROKERS`  | `localhost:29092`       |
| `KAFKA_TOPIC`    | `bifrost-traces`        |
| `KAFKA_UI_URL`   | `http://localhost:8090` |

## Stop Docker Services

```bash
cd "load test"
docker compose down          # stop containers
docker compose down -v       # also delete Kafka data volume
```

Stop dev Bifrost with Ctrl+C in the terminal running `start-dev.sh`.
