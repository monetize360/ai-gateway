# Semantic routing config examples

| File | Use |
|------|-----|
| [semantic-routing.local.json](./semantic-routing.local.json) | Loopback vLLM-SR on `:18080` for developer validation |
| [semantic-routing.prod.json](./semantic-routing.prod.json) | Shape for a deployed management API + inline overrides |

Merge the `config` object into the governance plugin entry in `config.json` (or tenant plugin config).

Local sidecar: `plugins/governance/poc/vllm_sr/start.sh`  
Operator docs: `docs/providers/routing-rules.mdx`
