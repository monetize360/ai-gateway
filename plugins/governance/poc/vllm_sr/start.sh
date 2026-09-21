#!/usr/bin/env bash
# Start the vLLM-SR management API on :18080 for Bifrost Step 2 (decision sidecar).
#
# CLI 0.3.0 emits providers.defaults.default_model, but the router image only
# accepts providers.defaults.model. So we add default_model for CLI validation,
# then rewrite it in the generated runtime config before the router loads it.
set -euo pipefail

export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"

REPO_CONFIG="$(cd "$(dirname "$0")" && pwd)/config.yaml"
WORKDIR="${VLLM_SR_WORKDIR:-/tmp/vllm-sr-bifrost}"
SRC_CONFIG="$WORKDIR/config.yaml"
RUNTIME_CONFIG="$WORKDIR/.vllm-sr/runtime-config.yaml"
LOG="$WORKDIR/serve.log"
DEFAULT_MODEL="${VLLM_SR_DEFAULT_MODEL:-gemini/gemini-2.5-flash-lite}"
PORT_OFFSET="${VLLM_SR_PORT_OFFSET:-10000}"
READY_URL="http://127.0.0.1:$((8080 + PORT_OFFSET))/ready"

if ! docker info >/dev/null 2>&1; then
  echo "Docker is not reachable. Start Docker Desktop and retry." >&2
  exit 1
fi

echo "[1/5] Stopping any running vllm-sr..."
vllm-sr stop >/dev/null 2>&1 || true

echo "[2/5] Writing source config with default_model for CLI validation..."
mkdir -p "$WORKDIR"
python3 - "$REPO_CONFIG" "$SRC_CONFIG" "$DEFAULT_MODEL" <<'PY'
import re, sys
src, dst, default_model = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(src).read()
text = re.sub(r"(?m)^  defaults:\n(?:    .*\n)*", "", text)
text = text.replace(
    "providers:\n",
    f"providers:\n  defaults:\n    default_model: {default_model}\n",
    1,
)
open(dst, "w").write(text)
PY

echo "[3/5] Starting vllm-sr (API on :$((8080 + PORT_OFFSET)))..."
cd "$WORKDIR"
rm -f "$RUNTIME_CONFIG"
VLLM_SR_PORT_OFFSET="$PORT_OFFSET" nohup vllm-sr serve --minimal --config "$SRC_CONFIG" >"$LOG" 2>&1 &
echo "  serve pid $!, log $LOG"

echo "[4/5] Rewriting providers.defaults in generated runtime config..."
for _ in $(seq 1 120); do
  grep -q 'default_model:' "$RUNTIME_CONFIG" 2>/dev/null && break
  sleep 1
done
if [ ! -f "$RUNTIME_CONFIG" ]; then
  echo "runtime config never generated; see $LOG" >&2
  exit 1
fi
python3 - "$RUNTIME_CONFIG" <<'PY'
import re, sys
path = sys.argv[1]
text = open(path).read()
patched = re.sub(r"(?m)^    default_model:", "    model:", text)
if patched != text:
    open(path, "w").write(patched)
    print("  rewrote default_model -> model")
else:
    print("  nothing to rewrite")
PY

echo "[5/5] Restarting router with patched config..."
docker restart vllm-sr-router-container >/dev/null 2>&1 || true

for _ in $(seq 1 90); do
  if curl -sf --max-time 2 "$READY_URL" >/dev/null 2>&1; then
    echo "READY: $READY_URL"
    curl -sS "$READY_URL"
    echo
    exit 0
  fi
  state=$(docker inspect -f '{{.State.Status}}' vllm-sr-router-container 2>/dev/null || echo missing)
  if [ "$state" = "exited" ] || [ "$state" = "dead" ]; then
    break
  fi
  sleep 2
done

echo "FAILED - router API never came up. Last router logs:" >&2
docker logs --tail 40 vllm-sr-router-container 2>&1 | tail -40 >&2
exit 1
