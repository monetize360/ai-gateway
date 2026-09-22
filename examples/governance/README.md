# Semantic routing config examples

| File | Use |
|------|-----|
| [semantic-routing.local.json](./semantic-routing.local.json) | In-process `semantic-router` plugin (`router.mode=plugin`) for local validation |
| [semantic-routing.prod.json](./semantic-routing.prod.json) | HTTP sidecar shape (`router.mode=http`) + inline overrides |
| [semantic-router-candle-domain.yaml](./semantic-router-candle-domain.yaml) | Canonical embedded recipe showing a Candle CPU learned-domain binding |

Also enable a top-level `plugins[]` entry for `semantic-router` with `recipe_file` (see root `config.json`). Merge the governance `config` object into the governance plugin entry.

Recipe: `plugins/governance/vllm_sr/config.yaml`

For learned classifiers, run `make build-semantic-router-native`, download the
recipe's pinned Python-exported model artifact under the configured `artifact`
path, and start Bifrost with `CGO_ENABLED=1`. Models are initialized once during
plugin startup; Python is not part of the request path.
Operator docs: `docs/providers/routing-rules.mdx`
