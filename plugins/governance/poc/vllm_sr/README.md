# vLLM Semantic Router as Bifrost Step 2

Bifrost keeps Layer 1 (virtual-key pool + capability filter) and the actual Gemini call.
This recipe is a **decision sidecar**. Bifrost calls `POST /api/v1/routing/preview` and then dispatches itself.

Do **not** send user traffic to Envoy `:8899` or `vllm-sr request chat`.

## Ports

vLLM-SR management API defaults to **8080**, which collides with Bifrost. Remap it before `vllm-sr serve`.

Example Bifrost plugin config:

```json
"semantic_routing": {
  "enabled": true,
  "default_for_all": true,
  "router": {
    "base_url": "http://127.0.0.1:18080",
    "preview_path": "/api/v1/routing/preview",
    "entrypoint": "vllm-sr/auto",
    "timeout_ms": 2000
  }
}
```

When `router.base_url` is set, Bifrost uses only the vLLM-SR preview for Layer 2
(the deprecated `classifier` config block is ignored).

Production checklist: [PROD-CHECKLIST.md](./PROD-CHECKLIST.md).
Example configs: `examples/governance/semantic-routing.local.json` and `.prod.json`.

## Starting the router

```bash
./start.sh
```

`start.sh` works around two mismatches between CLI 0.3.0 and the `latest` router image:

- the CLI emits `providers.defaults.default_model`, the image only accepts `providers.defaults.model`, so the generated runtime config is rewritten before the router loads it
- the management API binds to container loopback unless `global.services.management_api.bind_address` is set, which makes the published port unreachable

`config.yaml` also needs `backend_refs` on every model (the image requires them for any model a listener owns) and an explicit `algorithm.multi_factor` block. Only `weights`, `slo`, and `latency_percentile` are accepted by both the CLI validator and the image.

## Why the router needs signals

A config with a single catch-all decision returns the same model for every prompt. Its `multi_factor` algorithm reports `quality_disabled=true` because no quality index is loaded, and with no live latency/cost/load telemetry all candidates tie — so the first model in `modelRefs` always wins.

`config.yaml` therefore defines lexical and structural signals and one decision per prompt class. Embedding-backed signals (`complexity`, `embedding`, `domain`) are deliberately unused: they need a prepared embedding model, which this minimal deployment does not load.

| Decision | Priority | Fires on | First choice |
|---|---|---|---|
| `image-pool` | 400 | `input_modality: image` | `gemini-2.5-pro` |
| `long-context-pool` | 350 | `context` ≥ 1500 tokens | `gemini-2.5-pro` |
| `deep-reasoning-pool` | 300 | prove / derive / trade-offs keywords | `gemini-2.5-pro` |
| `code-pool` | 250 | code / debug / refactor keywords | `gemini-2.5-flash` |
| `summarization-pool` | 200 | summarize / tl;dr keywords | `gemini-2.0-flash` |
| `gemini-pool` | 100 | catch-all | `multi_factor` over all four |

Two schema details cost real debugging time: `context.min_tokens` and `max_tokens` must be **strings** (`"1500"`, `"1M"`), and the router only sees what Bifrost puts in `messages`. Bifrost truncates the preview payload at 8000 chars, so a 1M-token prompt still looks like ~2K tokens to the router — the `long_context` threshold is set to a value reachable under that cap, and Bifrost Layer 1 stays authoritative for actual context-window fit.

Bifrost forwards attachment parts with their payloads replaced by short markers (`data:inline`), so `input_modality` fires without shipping base64 image bytes to the sidecar.

## Preview

After the stack is ready (`GET http://127.0.0.1:18080/ready`), call the management API directly (same endpoint Bifrost uses). No Python helper is required.

The preview body is OpenAI-chat shaped. This build **rejects unknown fields** such as `eligible_models`, `candidates`, `request_type`, and `request_profile` with `400 INVALID_INPUT`, so Bifrost intersects the eligible pool client-side after the response.

```bash
curl -sS http://127.0.0.1:18080/ready

curl --location 'http://127.0.0.1:18080/api/v1/routing/preview' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "vllm-sr/auto",
    "messages": [{"role":"user","content":"Write a Python function that reverses a linked list."}]
  }'
```

Import `postman.json` into Postman for the same calls.

The response carries `selected_model`, `recommended_models`, `routing_decision`, `selection_status`, and `selection_method`.

Confirm field names on the installed build:

```bash
curl -sS 'http://127.0.0.1:18080/openapi.json?path=/api/v1/routing/preview&method=POST'
```

Bifrost always **intersects** `selected_model` (and any ranked candidates) with the Layer 1 pool. Unknown IDs are dropped; if nothing remains, selection fails open to the cost/preference ranker.

Register the same `provider/model` IDs here that the virtual key allowlists.
