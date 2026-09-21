# Governance PoC / local validation

This directory is for **local development and validation only**. It is **not** required to build or run the Bifrost production binary.

## What belongs here

| Path | Purpose |
|------|---------|
| `vllm_sr/` | Local vLLM Semantic Router recipe (`start.sh`, `config.yaml`, Postman). Bifrost calls `POST /api/v1/routing/preview` on the management API. |
| `gemini-model-routing.json` | Sample `model_overrides_file` catalog for local Gemini pools. |

## Production path (in the gateway binary)

- Layer 1: request profile + VK capability filter (`capability.go`, `requestprofile.go`)
- Layer 2: vLLM-SR preview client (`vllmsr.go`) when `semantic_routing.router.base_url` is set
- On router error / unset / out-of-pool: decline rewrite (keep incoming model)

See:

- `examples/governance/semantic-routing.local.json` — loopback validation
- `examples/governance/semantic-routing.prod.json` — deployed management API shape
- `docs/providers/routing-rules.mdx` — operator docs

## Local validation (preserved)

1. Start Docker Desktop.
2. `cd plugins/governance/poc/vllm_sr && ./start.sh`
3. `curl -sS http://127.0.0.1:18080/ready`
4. Point Bifrost `semantic_routing.router.base_url` at `http://127.0.0.1:18080` (see local example config).
5. Send a VK-backed chat request; logs should show `2/route: vllm-sr selected=...`.
