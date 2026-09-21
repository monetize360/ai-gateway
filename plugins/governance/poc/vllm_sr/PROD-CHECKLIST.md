# Production sidecar checklist (vLLM-SR + Bifrost)

Use with a deployed vLLM Semantic Router management API. Bifrost owns inference.

## Bifrost

- [ ] `semantic_routing.enabled=true`
- [ ] `router.base_url` points at the **management** API (not Envoy `:8899`)
- [ ] Prefer `action: "semantic"` routing rules over `default_for_all` in prod
- [ ] `router.timeout_ms` sized for your SLO (local work budget is ~10ms; total ≈ 10ms + timeout)
- [ ] `router.api_key` supplied via secret/env if the management API requires auth
- [ ] Inline `model_overrides` (or catalog) — avoid absolute `model_overrides_file` paths
- [ ] Virtual-key allowlist IDs match `provider/model` strings used in the sidecar config

## vLLM-SR

- [ ] Decisions/signals use the **same** `provider/model` IDs as Bifrost VK allowlists
- [ ] Management API reachable and `/ready` returns success
- [ ] Preview path is `/api/v1/routing/preview` (OpenAI-chat shaped body; no `eligible_models` field)
- [ ] Dummy `backend_refs` are fine if Bifrost still dispatches; do not send user traffic through Envoy for this design
- [ ] Monitor Bifrost fail-open rate (`2/route: vllm-sr failed` / outside L1 pool)

## Local validation

See `plugins/governance/poc/README.md` and `examples/governance/semantic-routing.local.json`.
