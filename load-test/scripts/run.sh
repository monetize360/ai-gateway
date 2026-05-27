#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# run.sh — Start Kafka + fake-llm, configure the Kafka connector on dev Bifrost,
#          and run the load test against the local dev server (not Docker Bifrost).
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${COMPOSE_DIR}/.." && pwd)"

BIFROST_URL="${BIFROST_URL:-http://localhost:8080}"
KAFKA_UI_URL="${KAFKA_UI_URL:-http://localhost:8090}"
KAFKA_BROKERS="${KAFKA_BROKERS:-localhost:29092}"
KAFKA_TOPIC="${KAFKA_TOPIC:-bifrost-traces}"
TOTAL_REQUESTS="${TOTAL_REQUESTS:-5000}"
CONCURRENCY="${CONCURRENCY:-50}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ─────────────────────────────────────────────────────────────────────────────
# 1. Pre-flight checks
# ─────────────────────────────────────────────────────────────────────────────
log_info "Checking dependencies…"

if ! command -v docker &>/dev/null; then
    log_error "docker not found. Please install Docker Desktop or Docker Engine."
    exit 1
fi

if ! docker compose version &>/dev/null 2>&1; then
    log_error "docker compose (v2) not found. Please upgrade Docker."
    exit 1
fi

if ! command -v python3 &>/dev/null; then
    log_error "python3 not found. Please install Python 3.10+."
    exit 1
fi

if ! python3 -c "import aiohttp" &>/dev/null 2>&1; then
    log_warn "aiohttp not found — installing…"
    pip3 install --quiet aiohttp
fi

if ! curl -sf --max-time 3 "${BIFROST_URL}/health" >/dev/null 2>&1; then
    log_error "Dev Bifrost is not running at ${BIFROST_URL}."
    echo ""
    echo "  Start it in another terminal (from the repo root):"
    echo "    APP_DIR=\"\$(pwd)/load-test/dev-config\" ./start-dev.sh"
    echo ""
    exit 1
fi

cleanup_conflicting_containers() {
    local name
    for name in kafka fake-llm kafka-ui; do
        if docker ps -a --format '{{.Names}}' | grep -qx "${name}"; then
            log_warn "Found existing container '${name}' — removing it to avoid name conflicts."
            docker rm -f "${name}" >/dev/null 2>&1 || {
                log_error "Could not remove conflicting container '${name}'."
                exit 1
            }
        fi
    done
}

# ─────────────────────────────────────────────────────────────────────────────
# 2. Start Kafka + fake-llm (no Docker Bifrost)
# ─────────────────────────────────────────────────────────────────────────────
log_info "Starting Kafka + fake-llm in ${COMPOSE_DIR}…"
cd "${COMPOSE_DIR}"

cleanup_conflicting_containers

docker compose pull --quiet --ignore-pull-failures 2>/dev/null || true
docker compose up -d --build

log_info "Waiting for supporting services…"

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
            docker compose logs --tail=30
            exit 1
        fi
        sleep 2
    done
    log_info "${name} is ready."
}

wait_for_service "http://localhost:8000/health" "fake-llm" 40
wait_for_service "${KAFKA_UI_URL}"              "Kafka UI"  40

# ─────────────────────────────────────────────────────────────────────────────
# 3. Configure Kafka connector and provider on dev Bifrost
# ─────────────────────────────────────────────────────────────────────────────
log_info "Configuring Kafka connector (brokers=${KAFKA_BROKERS}, topic=${KAFKA_TOPIC})…"
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
  -d "${kafka_payload}")
if [[ "${http_code}" != "200" ]]; then
    log_error "PUT /api/plugins/kafka returned HTTP ${http_code}"
    exit 1
fi

log_info "Ensuring fake-llm provider exists (http://localhost:8000)…"
provider_payload='{
  "provider": "fake-llm",
  "custom_provider_config": {
    "base_provider_type": "openai",
    "is_key_less": true
  },
  "network_config": {
    "base_url": "http://localhost:8000/",
    "default_request_timeout_in_seconds": 30,
    "max_retries": 0,
    "retry_backoff_initial": 500,
    "retry_backoff_max": 5000
  },
  "keys": []
}'
http_code=$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST "${BIFROST_URL}/api/providers" \
  -H "Content-Type: application/json" \
  -d "${provider_payload}" || echo "000")
if [[ "${http_code}" == "200" || "${http_code}" == "201" ]]; then
    log_info "fake-llm provider created."
elif [[ "${http_code}" == "409" ]]; then
    log_info "fake-llm provider already exists."
else
    log_error "POST /api/providers returned HTTP ${http_code}"
    exit 1
fi

# Pre-create topic so the idempotent producer does not fail on first write.
if docker ps --format '{{.Names}}' | grep -qx kafka; then
    log_info "Ensuring Kafka topic '${KAFKA_TOPIC}' exists…"
    docker exec kafka /opt/kafka/bin/kafka-topics.sh \
        --bootstrap-server localhost:9092 \
        --create --if-not-exists \
        --topic "${KAFKA_TOPIC}" \
        --partitions 1 \
        --replication-factor 1 >/dev/null 2>&1 || true
fi

log_info "Connector setup complete."

# ─────────────────────────────────────────────────────────────────────────────
# 4. Show service endpoints
# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────"
echo "  Service endpoints"
echo "────────────────────────────────────────────"
echo "  Bifrost (dev)      : ${BIFROST_URL}"
echo "  Fake LLM API       : http://localhost:8000"
echo "  Kafka broker       : ${KAFKA_BROKERS}"
echo "  Kafka topic        : ${KAFKA_TOPIC}"
echo "  Kafka UI           : ${KAFKA_UI_URL}"
echo "────────────────────────────────────────────"
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# 5. Run load test
# ─────────────────────────────────────────────────────────────────────────────
log_info "Running load test: ${TOTAL_REQUESTS} requests at concurrency ${CONCURRENCY}…"

python3 "${SCRIPT_DIR}/load_test.py" \
    --total        "${TOTAL_REQUESTS}" \
    --concurrency  "${CONCURRENCY}" \
    --bifrost-url  "${BIFROST_URL}" \
    --kafka-ui-url "${KAFKA_UI_URL}" \
    --kafka-topic  "${KAFKA_TOPIC}"

# ─────────────────────────────────────────────────────────────────────────────
# 6. Final instructions
# ─────────────────────────────────────────────────────────────────────────────
echo ""
log_info "Load test complete!"
echo ""
echo "  Bifrost UI:"
echo "  → ${BIFROST_URL}"
echo ""
echo "  Kafka UI:"
echo "  → ${KAFKA_UI_URL}"
echo ""
echo "  Browse Kafka trace records:"
echo "  → ${KAFKA_UI_URL}/ui/clusters/local/all-topics/${KAFKA_TOPIC}/messages"
echo ""
echo "  To stop Docker services:"
echo "  → cd \"${COMPOSE_DIR}\" && docker compose down"
echo ""
