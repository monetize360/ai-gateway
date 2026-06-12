#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# run.sh — Tenant-store load test orchestrator:
#   1. Start fake-llm (+ optional Kafka) via Docker Compose
#   2. Start Bifrost with repo-root config.json (tenant_store.enabled=true)
#   3. Seed the first MPilot tenant (global DB → tenant DB)
#   4. Run governance tests (routing, rate limit, budget)
#   5. Run the throughput load test with tenant JWT auth
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${COMPOSE_DIR}/.." && pwd)"

BIFROST_PORT="${BIFROST_PORT:-8091}"
BIFROST_URL="${BIFROST_URL:-http://localhost:${BIFROST_PORT}}"
FAKE_LLM_PORT="${FAKE_LLM_PORT:-18000}"
KAFKA_UI_URL="${KAFKA_UI_URL:-http://localhost:8090}"
KAFKA_BROKERS="${KAFKA_BROKERS:-localhost:29092}"
KAFKA_TOPIC="${KAFKA_TOPIC:-bifrost-traces}"
TOTAL_REQUESTS="${TOTAL_REQUESTS:-5000}"
CONCURRENCY="${CONCURRENCY:-50}"
FAKE_LLM_URL="${FAKE_LLM_URL:-http://localhost:${FAKE_LLM_PORT}/}"
RUN_LOAD_TEST="${RUN_LOAD_TEST:-1}"
RUN_GOVERNANCE_TEST="${RUN_GOVERNANCE_TEST:-1}"
START_KAFKA="${START_KAFKA:-0}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

ensure_python_deps() {
    if ! python3 -c "import aiohttp, jwt, psycopg2" &>/dev/null 2>&1; then
        log_warn "Installing Python deps from scripts/requirements.txt…"
        pip3 install --quiet -r "${SCRIPT_DIR}/requirements.txt"
    fi
}

wait_for_service() {
    local url="$1"
    local name="$2"
    local max_attempts="${3:-60}"
    local attempt=0
    log_info "Waiting for ${name} at ${url}…"
    until curl -sf --max-time 3 "${url}" >/dev/null 2>&1; do
        attempt=$(( attempt + 1 ))
        if [[ ${attempt} -ge ${max_attempts} ]]; then
            log_error "${name} did not become healthy after ${max_attempts} attempts."
            return 1
        fi
        sleep 2
    done
    log_info "${name} is ready."
}

# ─────────────────────────────────────────────────────────────────────────────
# 1. Pre-flight
# ─────────────────────────────────────────────────────────────────────────────
log_info "Checking dependencies…"
command -v docker >/dev/null || { log_error "docker not found"; exit 1; }
docker compose version >/dev/null 2>&1 || { log_error "docker compose v2 required"; exit 1; }
command -v python3 >/dev/null || { log_error "python3 not found"; exit 1; }
ensure_python_deps

if [[ ! -f "${REPO_ROOT}/config.json" ]]; then
    log_error "Missing ${REPO_ROOT}/config.json (tenant_store config)"
    exit 1
fi

# ─────────────────────────────────────────────────────────────────────────────
# 2. Docker: fake-llm (+ optional Kafka)
# ─────────────────────────────────────────────────────────────────────────────
log_info "Starting Docker services…"
cd "${COMPOSE_DIR}"

COMPOSE_SERVICES=(fake-llm)
if [[ "${START_KAFKA}" == "1" ]]; then
    COMPOSE_SERVICES+=(kafka kafka-ui)
fi

docker compose pull --quiet --ignore-pull-failures 2>/dev/null || true
docker compose up -d --build "${COMPOSE_SERVICES[@]}"

wait_for_service "http://localhost:${FAKE_LLM_PORT}/health" "fake-llm" 40
if [[ "${START_KAFKA}" == "1" ]]; then
    wait_for_service "${KAFKA_UI_URL}" "Kafka UI" 40
fi

# ─────────────────────────────────────────────────────────────────────────────
# 3. Seed first tenant (schema patches before Bifrost opens DB connections)
# ─────────────────────────────────────────────────────────────────────────────
log_info "Seeding first tenant (global DB → tenant DB)…"
python3 "${SCRIPT_DIR}/seed_tenant.py" --fake-llm-url "${FAKE_LLM_URL}"

# ─────────────────────────────────────────────────────────────────────────────
# 4. Start Bifrost with repo-root config.json (tenant store)
# ─────────────────────────────────────────────────────────────────────────────
BIFROST_STARTED_BY_US=0
if curl -sf --max-time 2 "${BIFROST_URL}/health" >/dev/null 2>&1; then
    log_warn "Bifrost already running at ${BIFROST_URL}."
    log_warn "If seed applied new columns, restart Bifrost to avoid PG cached-plan errors."
else
    log_info "Starting Bifrost (APP_DIR=${REPO_ROOT})…"
    cd "${REPO_ROOT}"
    PORT="${BIFROST_PORT}" APP_DIR="${REPO_ROOT}" ./start-dev.sh > /tmp/bifrost-load-test.log 2>&1 &
    BIFROST_PID=$!
    BIFROST_STARTED_BY_US=1
    log_info "Bifrost starting (pid ${BIFROST_PID})…"
    wait_for_service "${BIFROST_URL}/health" "Bifrost" 90 || {
        log_error "See /tmp/bifrost-load-test.log"
        exit 1
    }
fi

# ─────────────────────────────────────────────────────────────────────────────
# 5. Governance tests
# ─────────────────────────────────────────────────────────────────────────────

if [[ "${RUN_GOVERNANCE_TEST}" == "1" ]]; then
    log_info "Running governance integration tests…"
    if ! python3 "${SCRIPT_DIR}/governance_test.py" --bifrost-url "${BIFROST_URL}"; then
        log_error "Governance tests failed — see output above."
        exit 1
    fi
fi

# ─────────────────────────────────────────────────────────────────────────────
# 6. Optional Kafka connector (legacy flush metrics path)
# ─────────────────────────────────────────────────────────────────────────────
if [[ "${START_KAFKA}" == "1" ]]; then
    log_info "Configuring Kafka observability connector…"
    kafka_payload=$(cat <<EOF
{
  "enabled": true,
  "config": {
    "brokers": ["${KAFKA_BROKERS}"],
    "topic": "${KAFKA_TOPIC}",
    "client_id": "bifrost-load-test",
    "flush_frequency_ms": 100,
    "max_buffer_size": 10000
  }
}
EOF
)
    http_code=$(curl -s -o /dev/null -w "%{http_code}" \
      -X PUT "${BIFROST_URL}/api/plugins/kafka" \
      -H "Content-Type: application/json" \
      -d "${kafka_payload}" || echo "000")
    if [[ "${http_code}" != "200" ]]; then
        log_warn "PUT /api/plugins/kafka returned HTTP ${http_code} (non-fatal in tenant mode)"
    fi
fi

echo ""
echo "────────────────────────────────────────────"
echo "  Service endpoints"
echo "────────────────────────────────────────────"
echo "  Bifrost (tenant)   : ${BIFROST_URL}"
echo "  Fake LLM API       : http://localhost:${FAKE_LLM_PORT}"
echo "  MPilot global DB   : localhost:5432/mpilotv2 (tenant_store.global)"
echo "  Config             : ${REPO_ROOT}/config.json"
echo "────────────────────────────────────────────"
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# 7. Load test (tenant JWT)
# ─────────────────────────────────────────────────────────────────────────────
if [[ "${RUN_LOAD_TEST}" == "1" ]]; then
    log_info "Running load test: ${TOTAL_REQUESTS} requests @ concurrency ${CONCURRENCY}…"
    mkdir -p "${COMPOSE_DIR}/results"
    METRICS_FILE="${COMPOSE_DIR}/results/metrics-$(date +%Y%m%d-%H%M%S).json"

    MEASURE_FLUSH_FLAG=(--no-measure-flush)
    if [[ "${START_KAFKA}" == "1" ]]; then
        MEASURE_FLUSH_FLAG=(--measure-flush)
    fi

    python3 "${SCRIPT_DIR}/load_test.py" \
        --total "${TOTAL_REQUESTS}" \
        --concurrency "${CONCURRENCY}" \
        --bifrost-url "${BIFROST_URL}" \
        --model "fakellm-openai/gpt-4o-mini" \
        --tenant-auth \
        --config-json "${REPO_ROOT}/config.json" \
        "${MEASURE_FLUSH_FLAG[@]}" \
        --metrics-out "${METRICS_FILE}"
fi

echo ""
log_info "Done."
if [[ ${BIFROST_STARTED_BY_US} -eq 1 ]]; then
    echo "  Stop Bifrost: kill ${BIFROST_PID}"
fi
echo "  Stop fake-llm: cd \"${COMPOSE_DIR}\" && docker compose down"
