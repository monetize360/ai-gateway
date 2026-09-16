# HTTP transport

HTTP API layer for Monetize360 AI Gateway (`bifrost-http`).

## Build / run

From the repository root:

```bash
make setup-workspace
LOCAL=1 make build
PORT=8082 APP_DIR=. ./start-dev.sh
```

Health: `curl -fsS http://localhost:8082/health`

Docker images under this directory are API-only (no web UI).

Config schema: `config.schema.json` (also referenced from root `config.json`).
