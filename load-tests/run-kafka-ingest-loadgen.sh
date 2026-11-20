#!/usr/bin/env bash
# Standalone Kafka producer. Does not start or call the AI gateway.
#
# Topic and key match POST /v1/ingest/usage for the Lotus ingest loadgen:
#   topic = inference-usage_<tenant>
#   key   = loadgen-txn-<n>  (same value as externalTransactionId)
#   partitioner = sticky key (Kafka murmur2)
#
#   ./run-kafka-ingest-loadgen.sh --profile 5k --duration 60s
#   ./run-kafka-ingest-loadgen.sh --profile 10k --duration 60s
#   KAFKA_BOOTSTRAP_SERVERS=localhost:29092 ./run-kafka-ingest-loadgen.sh --profile 5k
#
# CPU and RSS are sampled once a second into kafka-ingest-loadgen-stats.csv,
# including the wait after produce. Pass --watch java to also sample the MPilot
# process. The gateway is refused if a bifrost process is already running.
#
# After the produce window the script waits until consumer group
# mpilot-usage-rating-<tenant> has lag 0 on the usage topic. That commit is
# the rating "publish event" step. The JSON summary then includes
# publish_elapsed_sec (produce end to lag 0) and total_sec (start to lag 0).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOADGEN_DIR="${SCRIPT_DIR}/kafka-ingest-loadgen"

PROFILE="5k"
DURATION="60s"
BROKERS="${KAFKA_BOOTSTRAP_SERVERS:-localhost:29092}"
TENANT_ID="${TENANT_ID:-b6c04e38-02f4-4c44-8e2e-44d38f489575}"
ORG_ID="${ORGANIZATION_ID:-5c84e8ca-68c0-454f-83dd-7e995e7704e5}"
WORKERS="64"
WATCH=""
RPS=""
PUBLISH_TIMEOUT="10m"
SKIP_PUBLISH_WAIT=0

usage() {
  cat <<'EOF'
Standalone Kafka usage producer (no gateway).

  ./run-kafka-ingest-loadgen.sh --profile 5k --duration 60s
  ./run-kafka-ingest-loadgen.sh --profile 10k --duration 60s

Flags:
  --profile 5k|10k   target rate (default 5k). --rps overrides this.
  --rps N            explicit records per second
  --duration D       e.g. 60s (default 60s)
  --brokers HOSTS    default localhost:29092 or KAFKA_BOOTSTRAP_SERVERS
  --tenant-id ID     default b6c04e38-02f4-4c44-8e2e-44d38f489575 (local tenant2)
  --org-id ID        organizationId on each record
  --c N              producer goroutines (default 64)
  --watch SUBSTR     also sample matching process CPU/RSS (not the gateway)
  --publish-timeout D
                     wait this long after produce for rating lag 0 (default 10m)
  --skip-publish-wait
                     print the produce summary without waiting for rating lag
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --profile) PROFILE="$2"; shift 2 ;;
    --rps) RPS="$2"; shift 2 ;;
    --duration) DURATION="$2"; shift 2 ;;
    --brokers) BROKERS="$2"; shift 2 ;;
    --tenant-id) TENANT_ID="$2"; shift 2 ;;
    --org-id) ORG_ID="$2"; shift 2 ;;
    --c) WORKERS="$2"; shift 2 ;;
    --watch) WATCH="$2"; shift 2 ;;
    --publish-timeout) PUBLISH_TIMEOUT="$2"; shift 2 ;;
    --skip-publish-wait) SKIP_PUBLISH_WAIT=1; shift ;;
    *) echo "Unknown arg: $1" >&2; usage; exit 1 ;;
  esac
done

export GOWORK=off
cd "${LOADGEN_DIR}"

args=(
  -profile "${PROFILE}"
  -duration "${DURATION}"
  -brokers "${BROKERS}"
  -tenant-id "${TENANT_ID}"
  -org-id "${ORG_ID}"
  -c "${WORKERS}"
  -watch "${WATCH}"
  -publish-timeout "${PUBLISH_TIMEOUT}"
)
if [[ "${SKIP_PUBLISH_WAIT}" == 1 ]]; then
  args+=(-skip-publish-wait)
fi
if [[ -n "${RPS}" ]]; then
  args+=(-rps "${RPS}")
fi

exec go run . "${args[@]}"
