# Monetize360 AI Gateway

LLM inference runtime for **MPilot FinOps**.

MPilot owns governance config (budgets, rate limits, virtual keys, routing).  
This service loads that config via `tenant_store` and serves an OpenAI-compatible HTTP API to providers.

```text
MPilot API  ──AiGatewayClient──►  AI Gateway (:8082)  ──►  LLM providers
     │                                  │
     └── FinOps / Simulator UI          └── tenant_store + logs
```

---

## Prerequisites

| Tool | Notes |
|------|--------|
| Go **1.26.2+** | Required to build / run — install manually |
| `make` | Required — install manually |
| Rust / `cargo`, `cmake`, C compiler | Installed automatically when missing |
| Docker | Postgres + Keycloak for MPilot |
| Sibling checkout | `mpilotv2/` next to `ai-gateway/` |

The in-process `semantic-router` plugin links **gitignored** Rust libraries under `semantic-router/*/target/release/`. They are **not** in git.

`./start-dev.sh` and `make dev` / `make build` run `make ensure-semantic-router-native`, which:

1. calls `make ensure-native-toolchain` (`scripts/ensure-native-toolchain.sh`) to install a missing C compiler, `cmake`, or Rust, and
2. builds the native libraries once (a few minutes), skipping the build when they already exist.

Use `TOOLCHAIN_AUTO_INSTALL=0` to report missing tools instead of installing them. Docker builds the same libraries inside `transports/Dockerfile.local`.

---

## Local development

Use **four terminals**. Do **not** bind the gateway to `:8080` (that port is the MPilot API).

### 1. Infra

```bash
cd ../mpilotv2/backend
bash clean-restart-services.sh
```

### 2. AI Gateway (`:8082`)

```bash
cd ../ai-gateway
# First clone only: installs Rust natives if missing (or: make build-semantic-router-native)
PORT=8082 APP_DIR=. ./start-dev.sh
```

`APP_DIR=.` loads root `config.json` (`tenant_store` + JWT keys). Without it, MPilot integration will not work.

```bash
curl -fsS http://localhost:8082/health
# {"status":"ok", ...}
```

### 3. MPilot API (`:8080`)

```bash
cd ../mpilotv2/backend
./mvnw -pl api spring-boot:run \
  -Dspring-boot.run.jvmArguments="--add-opens java.base/java.util=ALL-UNNAMED --add-opens java.base/java.lang=ALL-UNNAMED"
```

### 4. MPilot frontend (`:3000`)

```bash
cd ../mpilotv2/frontend
npm run dev
```

Interactive testing uses the MPilot **AI Gateway Simulator** — there is no bundled gateway web UI.

---

## Ports

| Service | Port | Notes |
|---------|------|--------|
| MPilot API | `8080` | |
| AI Gateway | **`8082`** | Default in MPilot (`app.ai-gateway.base-url`) |
| MPilot frontend | `3000` | Simulator lives here |
| Gateway `make dev` default | `8080` | Conflicts — always override to `8082` |

---

## Build

```bash
make setup-workspace
LOCAL=1 make build   # also ensures Semantic Router natives
# → tmp/bifrost-http
```

Prefer `LOCAL=1` / `./start-dev.sh` so builds use the local `go.work` modules.

Manual native rebuild (after deleting `semantic-router/*/target` or changing binding sources):

```bash
make build-semantic-router-native
```

Docker images under `transports/` are **API-only** (no web UI stage):

- `transports/Dockerfile`
- `transports/Dockerfile.local` (builds Semantic Router `.so` in a Rust stage)

---

## Repository layout

```text
ai-gateway/
├── core/                 # Engine + providers
├── framework/            # Config, log, vector, tenant stores
├── transports/
│   └── bifrost-http/     # HTTP API (historical path name — do not rename lightly)
│       └── ui/.gitkeep   # Embed stub only (no UI assets)
├── plugins/              # Governance, semantic-router, logging, …
├── semantic-router/      # Nested routing core + Rust bindings (natives in */target/)
├── config.json           # Local MPilot-integrated config
└── start-dev.sh          # Hot-reload API entrypoint (ensures natives)
```

---

## Notes

- Go module import paths may still use historical names for build compatibility. Do not rename them unless explicitly requested.
- License: see [LICENSE](LICENSE).
