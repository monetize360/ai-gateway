#!/bin/sh
# Restart the NeMo server when a config file under /configs changes.
set -eu
CONFIG_DIR="${NEMO_CONFIG_DIR:-/configs}"

fingerprint() {
  find "$CONFIG_DIR" -type f -exec stat -c '%Y %n' {} \; 2>/dev/null | sort | cksum || true
}

while true; do
  nemoguardrails server --config "$CONFIG_DIR" --port 8084 &
  pid=$!
  last=$(fingerprint)
  while kill -0 "$pid" 2>/dev/null; do
    sleep 5
    now=$(fingerprint)
    if [ "$now" != "$last" ]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      break
    fi
  done
  wait "$pid" 2>/dev/null || true
done
