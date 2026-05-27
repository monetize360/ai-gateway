#!/usr/bin/env bash
# Start Bifrost local development (UI on :3000, API + UI proxy on :8080).
#
# Usage:
#   ./start-dev.sh
#   ./start-dev.sh PORT=9090
#   DEBUG=1 ./start-dev.sh
#
# Stop with Ctrl+C.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# Go + dev tools (air, etc.) — extend if your install paths differ
export PATH="${HOME}/go-install/go/bin:${HOME}/go/bin:${PATH}"

# Node via nvm when available (same behavior as Makefile USE_NODE)
NVM_SH="${NVM_DIR:-$HOME/.nvm}/nvm.sh"
if [[ ! -s "$NVM_SH" ]]; then
	brew_prefix="$(brew --prefix nvm 2>/dev/null || true)"
	[[ -n "$brew_prefix" ]] && NVM_SH="${brew_prefix}/nvm.sh"
fi
if [[ -s "$NVM_SH" ]]; then
	# shellcheck source=/dev/null
	. "$NVM_SH"
	nvm install >/dev/null 2>&1 || true
	nvm use >/dev/null 2>&1 || true
fi

# Optional local secrets (Makefile default when USE_INFISICAL is not set)
if [[ -f "${ROOT}/.env" ]]; then
	set -a
	# shellcheck source=/dev/null
	. "${ROOT}/.env"
	set +a
fi

if ! command -v go >/dev/null 2>&1; then
	echo "error: go not found on PATH. Install Go or set PATH to include go-install/go/bin" >&2
	exit 1
fi

if ! command -v make >/dev/null 2>&1; then
	echo "error: make not found" >&2
	exit 1
fi

echo "Starting Bifrost dev environment from ${ROOT}"
echo "  App:  http://localhost:${PORT:-8080}"
echo "  UI:   http://localhost:3000 (Vite, proxied via API in dev)"
echo ""

exec make dev "$@"
