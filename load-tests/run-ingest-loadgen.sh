#!/usr/bin/env bash
# fasthttp loadgen for POST /v1/ingest/usage (not k6).
#
#   export MPILOT_ACCESS_TOKEN='<TENANTADMIN jwt>'
#   ./run-ingest-loadgen.sh --rps 20000 --duration 60s
#   ./run-ingest-loadgen.sh --rps 90000 --duration 20s

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOADGEN_DIR="${SCRIPT_DIR}/ingest-loadgen"

URL="${URL:-http://127.0.0.1:8082/v1/ingest/usage}"
RPS="${RPS:-20000}"
DURATION="${DURATION:-60s}"
CONCURRENCY="${CONCURRENCY:-8000}"
TOKEN="${MPILOT_ACCESS_TOKEN:-${AUTH_TOKEN:-}}"

usage() {
  cat <<'EOF'
fasthttp usage ingest loadgen.

Usage:
  export MPILOT_ACCESS_TOKEN='<TENANTADMIN jwt>'
  ./run-ingest-loadgen.sh --rps 20000 --duration 60s
  ./run-ingest-loadgen.sh --rps 90000 --duration 20s

Flags:
  --url URL          default http://127.0.0.1:8082/v1/ingest/usage
  --rps N            target RPS (default 20000)
  --duration D       e.g. 20s, 60s (default 60s)
  --c N              worker goroutines (default 8000)
  --token TOKEN      or MPILOT_ACCESS_TOKEN / AUTH_TOKEN
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --url) URL="$2"; shift 2 ;;
    --rps) RPS="$2"; shift 2 ;;
    --duration) DURATION="$2"; shift 2 ;;
    --c) CONCURRENCY="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    *) echo "Unknown arg: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ -z "${TOKEN}" ]]; then
  echo "MPILOT_ACCESS_TOKEN is required." >&2
  exit 1
fi

export GOWORK=off
cd "${LOADGEN_DIR}"
exec go run . \
  -url "${URL}" \
  -token "${TOKEN}" \
  -rps "${RPS}" \
  -duration "${DURATION}" \
  -c "${CONCURRENCY}"
