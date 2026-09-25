#!/usr/bin/env bash
# Start AI gateway local development (API hot reload).
#
# Usage:
#   ./start-dev.sh
#   PORT=8082 APP_DIR=. ./start-dev.sh
#   DEBUG=1 PORT=8082 APP_DIR=. ./start-dev.sh
#
# For MPilot integration use PORT=8082 and APP_DIR=. so config.json
# (tenant_store) loads from the repo root.
#
# Stop with Ctrl+C.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# Go + dev tools (air, etc.) — extend if your install paths differ.
# ~/.cargo/bin covers a rustup install that has not been picked up by the shell
# profile yet, so the first run after installing Rust still finds cargo.
export PATH="${HOME}/go-install/go/bin:${HOME}/go/bin:${HOME}/.cargo/bin:${PATH}"

# Embedded Semantic Router native runtimes (Candle/NLP/ML/ONNX). Built libs are
# gitignored under */target/release — ensure-semantic-router-native builds them
# once on a fresh clone before hot-reload starts.
SEMANTIC_ROUTER_ROOT="${SEMANTIC_ROUTER_ROOT:-${ROOT}/semantic-router}"
NATIVE_LIBRARY_DIRS=(
	"${SEMANTIC_ROUTER_ROOT}/candle-binding/target/release"
	"${SEMANTIC_ROUTER_ROOT}/ml-binding/target/release"
	"${SEMANTIC_ROUTER_ROOT}/nlp-binding/target/release"
	"${SEMANTIC_ROUTER_ROOT}/onnx-binding/target/release"
)
NATIVE_LIBRARY_PATH="$(IFS=:; echo "${NATIVE_LIBRARY_DIRS[*]}")"
export DYLD_LIBRARY_PATH="${NATIVE_LIBRARY_PATH}${DYLD_LIBRARY_PATH:+:${DYLD_LIBRARY_PATH}}"
export LD_LIBRARY_PATH="${NATIVE_LIBRARY_PATH}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
export CGO_ENABLED="${CGO_ENABLED:-1}"

# Optional local secrets (Makefile default when USE_INFISICAL is not set)
if [[ -f "${ROOT}/.env" ]]; then
	set -a
	# shellcheck source=/dev/null
	. "${ROOT}/.env"
	set +a
fi

if ! command -v go >/dev/null 2>&1; then
	echo "error: go not found on PATH. Install Go 1.26.2+ from https://go.dev/dl/" >&2
	exit 1
fi

if ! command -v make >/dev/null 2>&1; then
	echo "error: make not found" >&2
	exit 1
fi

# Install the C compiler / cmake / Rust toolchain when missing, then build the
# native libraries. Both steps are no-ops once everything is in place.
# Set TOOLCHAIN_AUTO_INSTALL=0 to only report missing tools.
echo "Ensuring Semantic Router native libraries..."
make ensure-semantic-router-native

# Ensure embed stub exists for //go:embed all:ui
mkdir -p transports/bifrost-http/ui
if [[ ! -f transports/bifrost-http/ui/.gitkeep && ! -f transports/bifrost-http/ui/.tmp ]]; then
	touch transports/bifrost-http/ui/.gitkeep
fi

echo "Starting AI gateway from ${ROOT}"
echo "  App:  http://localhost:${PORT:-8080}"
if [[ -n "${APP_DIR:-}" ]]; then
	echo "  Dir:  ${APP_DIR}"
fi
if [[ ! -f "${ROOT}/ui/package.json" ]]; then
	echo "  Mode: API-only (ui/package.json missing)"
fi
echo ""

exec make dev "$@"
