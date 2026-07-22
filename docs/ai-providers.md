# AI providers

The dashboard's AI analysis is provider-agnostic. The fetcher supports OpenAI
Chat Completions and Responses over HTTPS. Both paths require function calling:
Chat endpoints exchange `tools` and `tool_calls`, while Responses endpoints
exchange function-call items and `function_call_output` items. GitHub Copilot,
OpenAI, Nvidia Dynamo / NIMs, vLLM, Ollama, and compatible proxies can work when
the selected model supports the configured API contract. There is **no default provider**: you
must configure an endpoint and model explicitly (in `project.yaml` or via env),
or AI analysis fails fast with a clear error.

Configure your provider in your consumer repo's `project.yaml` under `ai:`:

```yaml
ai:
  api: "chat_completions"  # or responses
  endpoint: "..."         # Required. URL for the selected API.
  model: "..."            # Required. Model identifier the endpoint expects.
  headers:                # Optional. Extra HTTP headers merged into every call.
    Some-Header: "value"
```

Both `endpoint` and `model` are required when AI is enabled; provide them here
or via the `AI_ENDPOINT` / `AI_MODEL` env vars (see below). For GitHub Copilot,
the endpoint is `https://api.githubcopilot.com/chat/completions`.

Set the bearer token via the `AI_TOKEN` secret in the GitHub Actions workflow
(see the [reusable workflow README](../README.md)). The token is sent as
`Authorization: Bearer <AI_TOKEN>` unless an entry in `headers:` overrides it.

### Hiding the model identifier and endpoint URL from the public repo

`project.yaml` is committed to a public repo, so any value you put in
`endpoint:` or `model:` is visible to the world. That's fine for public
providers and standard model names, but it's a problem when:

- The model identifier is one you would rather not commit to a public file
  (a preview label, or a model only your org is enrolled in).
- The endpoint is a private gateway URL you don't want indexed.

For those cases, leave `endpoint:` and `model:` out of `project.yaml` and
pass them through repo-scoped GitHub Actions **variables** (not secrets;
these aren't sensitive enough to need masking). The reusable workflow
accepts `ai-model` and `ai-endpoint` inputs and forwards them to the
fetcher as `AI_MODEL` / `AI_ENDPOINT` env vars; the fetcher reads those
when the yaml fields are blank.

```yaml
# In the consumer repo's project.yaml
ai:
  # endpoint and model intentionally omitted; supplied via repo
  # variables on the consumer (see the deploy workflow).
```

```yaml
# In the consumer's .github/workflows/deploy.yml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .
      ai-model: ${{ vars.AI_MODEL }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

Set the variables once per consumer repo:

```sh
gh variable set AI_MODEL    --repo your-org/your-consumer-repo
gh variable set AI_ENDPOINT --repo your-org/your-consumer-repo
```

Resolution order is `project.yaml` field > env var, with no engine default, so
yaml entries still win if you ever need to override per-repo. Sourcing
the inputs from `vars.*` (instead of hardcoding in the workflow file)
keeps the values out of the public repo source.

On the [Kubernetes-native](kubernetes.md) path, set the endpoint and model in the
Helm values (`ai.endpoint` / `ai.model`) and the token via `--set ai.token=` or
`ai.existingSecret` instead of workflow secrets.

The engine also scrubs `ai.endpoint`, `ai.model`, and per-failure
`ai_analysis.model` from every JSON file written to `frontend/public/data/`
regardless of where the values came from, so private model labels never
reach the deployed GitHub Pages site even if a future change accidentally
puts them back in yaml.

## GitHub Copilot

Set `endpoint` to the Copilot chat-completions URL. `AI_TOKEN` is a fine-grained
PAT with the `copilot_chat` user permission. Set `model` explicitly to a model
your Copilot plan exposes; a public model id keeps the config reproducible
for anyone reading the repo:

```yaml
ai:
  endpoint: "https://api.githubcopilot.com/chat/completions"
  model: "claude-sonnet-4.6"
```

Copilot is metered, not free: it requires a subscription, and a full cold
fetch (one agentic investigation per failure) consumes request and token
allowance. The free individual tier works for trying it out but has a limited
monthly allowance; organizations need paid licenses. Model availability shifts
over time and varies by plan, so set `model` explicitly to one your plan
exposes (current options include the Claude Sonnet and GPT-5 families). Both
`endpoint` and `model` are required; the engine has no built-in default.

The fetcher automatically sends `Copilot-Integration-Id: copilot-developer-cli`
when (and only when) the endpoint's host is `*.githubcopilot.com`.

## OpenAI

Responses example:

```yaml
ai:
  api: responses
  endpoint: "https://api.openai.com/v1/responses"
  model: "<model-id>"
```

Responses requests use `store: false` and preserve reasoning items across tool calls.

```yaml
ai:
  endpoint: "https://api.openai.com/v1/chat/completions"
  model: "gpt-5-mini"
```

`AI_TOKEN` is your OpenAI API key.

## Azure OpenAI

Azure OpenAI commonly uses a per-deployment URL and an `api-key` header instead
of `Authorization: Bearer`. The engine can send custom headers, but it does not
interpolate secrets into `project.yaml` header values:

```yaml
ai:
  endpoint: "https://my-resource.openai.azure.com/openai/deployments/gpt-5-mini/chat/completions?api-version=2024-08-01-preview"
  model: "gpt-5-mini"
  headers:
    api-key: "<literal-key>"
```

Do not commit a real key in a public consumer repository. The stock reusable
workflow supports `AI_TOKEN` only as a bearer token and does not have a
secret-header input. Use a trusted proxy that translates the bearer token to an
Azure `api-key`, or customize the deployment to inject a private project config.
The selected Azure deployment must also support function calling.

## Nvidia Dynamo / NIM

NIMs accept the OpenAI schema. Use the model name your NIM exposes:

```yaml
ai:
  endpoint: "https://integrate.api.nvidia.com/v1/chat/completions"
  model: "meta/llama-3.1-70b-instruct"
```

`AI_TOKEN` is your NVIDIA API key. For self-hosted NIMs, point `endpoint` at
your cluster's gateway and add any routing headers your gateway expects.

## vLLM / Ollama / self-hosted

Any OpenAI-compatible server works. For Ollama:

```yaml
ai:
  endpoint: "http://localhost:11434/v1/chat/completions"
  model: "llama3.1"
```

Self-hosted endpoints typically don't require a token; set `AI_TOKEN` to any
non-empty placeholder in your workflow so the env check in the fetcher passes.

## Ray Serve (KubeRay)

Ray Serve's LLM app (`ray.serve.llm:build_openai_app`) serves the OpenAI schema
on vLLM, so it works unchanged. With the default `route_prefix: "/"` the
chat-completions path is `/v1/chat/completions`:

```yaml
ai:
  endpoint: "http://<serve-svc>.<namespace>.svc.cluster.local:8000/v1/chat/completions"
  model: "moonshotai/Kimi-K2-Instruct-0905"   # must equal the RayService model_id
```

Notes:

- `model` must match the serve config's `model_loading_config.model_id`
  exactly, and is what `/v1/models` reports.
- The app has no auth by default; set `AI_TOKEN` to any non-empty placeholder.
- Function calling requires `enable_auto_tool_choice: true` plus a
  `tool_call_parser` for the model (e.g. `kimi_k2`, `hermes`) in the serve
  `engine_kwargs`. Without it the agentic loop cannot run.
- Context-window auto-sizing works: Ray reports the window under
  `metadata.max_request_context_length` in `/v1/models`, which the fetcher
  reads (alongside top-level `context_window` / `max_model_len`).

## Cache invalidation when switching providers

Each cached analysis records a fingerprint of the model and endpoint that
produced it. Changing either value automatically marks the entry stale and
reanalyzes it on the next fetch. The prior result stays published until a
replacement succeeds, so a slow or failing provider swap does not erase the
last usable analysis.

The engine always runs the agentic loop. Use `clear-cache.yml` only when you
want an immediate full rebaseline rather than incremental refresh.

## Function-calling support (required)

The engine sends an OpenAI-style `tools` field on every request and expects
`tool_calls` back from the model. There is no tools-free fallback: the first
agentic call to an endpoint that returns HTTP 400/422 with a tools-related
error is treated as a capability miss, and every failure that run surfaces as
an "AI analysis unavailable" summary (the fetcher logs `AI endpoint rejected
tools`). Verified endpoints: GitHub Copilot, OpenAI, Azure OpenAI, Ray Serve
(Kimi-K2 / Qwen2.5 with a tool-call parser), and tool-calling Ollama / NIM
models (per-model).

## Cost and latency notes

Each non-transient failure triggers one agentic investigation (a sequence of
chat-completion calls). Roughly 50-150k input tokens and 30-90 seconds of
wall clock per failure, depending on artifact size and how deep the model
digs. Most providers price the input dominant. See
[agentic.md](agentic.md) for cost-control knobs (`max_iters`, `concurrency`).
