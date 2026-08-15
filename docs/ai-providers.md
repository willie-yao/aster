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
or via the `AI_ENDPOINT` / `AI_MODEL` env vars (see below). GitHub Copilot has
separate Responses and Chat Completions endpoints. Choose the API listed in the
model catalog's `supported_endpoints` field.

### Reasoning effort

Set the optional provider reasoning effort with `AI_REASONING_EFFORT` or Helm
`ai.reasoningEffort`. Empty or unset uses the provider default. The engine
normalizes whitespace and case and accepts `none`, `low`, `medium`, `high`,
`xhigh`, and `max`. Unknown values fail before provider I/O. The engine does not
infer effort from the model name and does not downgrade `max` to `xhigh`.

The native request fields are:

```json
// Responses
{"reasoning":{"effort":"high"}}
```

```json
// Chat Completions
{"reasoning_effort":"high"}
```

Provider and model support is not universal. The captured Copilot catalog used
for the current compatibility tests advertises `none`, `low`, `medium`, `high`,
and `xhigh` for GPT-5.4, but not `max`. GPT-5.6 Sol advertises all six values,
including `max`, and is exposed through Copilot Responses rather than Chat
Completions. Copilot Opus 4.8 is exposed through Chat Completions and
`/v1/messages`; the engine does not have a proven reasoning-effort mapping for
that model, and native Anthropic Messages support is outside this contract.

Higher effort can increase latency and token use. Requested effort is private,
content-free provenance and is separate from provider-reported reasoning-token
counts. The dashboard does not publish or retain provider chain-of-thought or
encrypted reasoning payloads. A non-empty effort changes model and cache
fingerprints; empty preserves the historical request body and fingerprint.

The guided `aster onboard` wizard includes coordinate presets for GitHub
Copilot Responses, GitHub Copilot Chat Completions, OpenAI Responses, OpenAI
Chat Completions, and the public NVIDIA API.
It also provides guided self-hosted, Azure, custom, and configure-later paths.
Preset endpoints remain editable. Models are never preset because availability
varies by account and deployment. Selecting a preset does not test credentials,
subscription access, network reachability, or function-calling support.

Set the bearer token via the `AI_TOKEN` secret in the GitHub Actions workflow
(see the [reusable workflow README](../README.md)). The token is sent as
`Authorization: Bearer <AI_TOKEN>` unless an entry in `headers:` overrides it.

Onboarding prompt authoring does not use these deployment credentials. Agent
mode uses the selected credential from the user's existing OpenCode
configuration; handoff and TODO-template modes do not run a model.

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
accepts `ai-model`, `ai-endpoint`, and `ai-reasoning-effort` inputs and forwards them to the
fetcher as `AI_MODEL`, `AI_ENDPOINT`, and `AI_REASONING_EFFORT` env vars; the fetcher reads those
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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.2
    with:
      project_dir: .
      ai-model: ${{ vars.AI_MODEL }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-reasoning-effort: ${{ vars.AI_REASONING_EFFORT }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

Set the variables once per consumer repo:

```sh
gh variable set AI_MODEL    --repo your-org/your-consumer-repo
gh variable set AI_ENDPOINT --repo your-org/your-consumer-repo
# Optional; omit for the provider default.
gh variable set AI_REASONING_EFFORT --body high --repo your-org/your-consumer-repo
```

Endpoint, model, and API resolution order is `project.yaml` field > env var,
with no engine default, so yaml entries still win for those coordinates.
Reasoning effort is deployment-owned and comes only from `AI_REASONING_EFFORT`
or Helm `ai.reasoningEffort`. Sourcing
the inputs from `vars.*` (instead of hardcoding in the workflow file)
keeps the values out of the public repo source.

On the [Kubernetes-native](kubernetes.md) path, set the endpoint and model in the
Helm values (`ai.endpoint` / `ai.model` / `ai.reasoningEffort`) and the token via `--set ai.token=` or
`ai.existingSecret` instead of workflow secrets.

The engine also scrubs `ai.endpoint`, `ai.model`, and per-failure
`ai_analysis.model` from every JSON file written to `frontend/public/data/`
regardless of where the values came from, so private model labels never
reach the deployed GitHub Pages site even if a future change accidentally
puts them back in yaml.

## GitHub Copilot

Use Responses for models whose catalog entry lists only `/responses`:

```yaml
ai:
  api: "responses"
  endpoint: "https://api.githubcopilot.com/responses"
  model: "gpt-5.6-sol"
```

Use Chat Completions only when the model advertises `/chat/completions`:

```yaml
ai:
  api: "chat_completions"
  endpoint: "https://api.githubcopilot.com/chat/completions"
  model: "claude-sonnet-4.6"
```

The Responses endpoint is live-verified with text output, strict structured
output, forced function tools, and `function_call_output` continuation. In both
modes, `AI_TOKEN` is a fine-grained PAT with the `copilot_chat` user permission.
Set `model` explicitly to a model your Copilot plan exposes; a public model id
keeps the config reproducible for anyone reading the repo.

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

## Cache provenance when switching providers

Each cached analysis records a fingerprint of the model and endpoint that
produced it. Changing either value affects new analyses only. Existing reusable
entries remain cached with their original fingerprint.

The engine always runs the agentic loop. Use `ai.cache_generation`,
`AI_CACHE_GENERATION`, the Pages `ai-cache-generation` input, or Helm
`analysisCache.generation` for a reversible rebaseline. Use `clear-cache.yml`
only for emergency destructive cleanup.

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

## Usage metadata

The Chat Completions adapter reads prompt/input and completion/output token
fields plus cached-input and reasoning detail fields when present. The
Responses adapter reads input, output, cached-input, and reasoning token fields.
Providers may omit usage. The dashboard does not substitute a tokenizer
estimate and does not ship vendor price tables.

## Agent Sandbox OpenCode provider access

Agent Sandbox remains experimental and disabled by default. After an OpenCode
Agent Sandbox runtime is explicitly enabled, `direct` is the default credential
mode. `gateway` remains available as an explicit tokenless mode. Agent Sandbox
OpenCode supports two separate native protocols:

| `model_provider.api` | OpenCode provider package |
|---|---|
| `chat_completions` | `@ai-sdk/openai-compatible` |
| `responses` | `@ai-sdk/openai` |

Configure a full operation endpoint. Chat Completions endpoints must end with
`/chat/completions`; Responses endpoints must end with `/responses`. The engine
derives only the provider base URL and rejects API/path mismatches instead of
guessing.

Direct mode can use either:

- `auth.type: bearer`, which exposes one dedicated inference credential to the
  OpenCode executor through the fixed `PROW_AI_MODEL_PROVIDER_TOKEN`
  environment variable; or
- `auth.type: none`, which renders no Secret reference and is suitable only for
  an endpoint that intentionally requires no authentication.

With pinned OpenCode 1.18.2, native Responses requires direct bearer auth. The
`@ai-sdk/openai` package requires an API key before it starts a request, so
Responses with `auth.type: none` or tokenless gateway mode fails validation
rather than inventing a placeholder credential. Direct unauthenticated access
and gateway mode remain supported for Chat Completions, including internal Ray
Serve deployments.

Pinned source tag `v1.18.2` at commit
`70b56a0a93d366889cae950379cc9d2537148fa2` uses
`@ai-sdk/openai` 3.0.84 and `@ai-sdk/openai-compatible` 2.0.41. Its model
`options.reasoningEffort` reaches `reasoning.effort` for Responses and
`reasoning_effort` for Chat Completions. Deterministic fake-provider tests
capture both actual OpenCode requests. The pinned OpenAI protocol excludes
`max`, so Agent Sandbox `modelProvider.reasoningEffort` supports empty,
`none`, `low`, `medium`, `high`, and `xhigh`, but rejects `max`. Engine-native
GPT-5.6 Sol Responses requests still support `max` when the provider does.

For bearer auth, the named Secret must already exist in the Agent Sandbox
execution namespace. The chart never creates, copies, reads, or prints it. The
dashboard receives only the exact Secret name and key so it can construct the
Sandbox Pod. The Pod uses one `secretKeyRef`; `envFrom`, Secret volumes,
projected tokens, extra credential references, and arbitrary environment
variables remain denied by admission. Use a dedicated inference-only
credential. Do not reuse `BOT_TOKEN`, `FIX_TOKEN`, OAuth credentials, GitHub
read credentials, or a general GitHub PAT.

OpenCode 1.18.2 receives `apiKey: "{env:PROW_AI_MODEL_PROVIDER_TOKEN}"` in its
isolated configuration for direct bearer mode. The credential value is not
serialized into `opencode.json`, the execution request, runtime identity,
annotations, labels, hashes, telemetry, logs, evidence, or results. Before a
result is published, the executor rejects stdout, stderr, structured analysis,
summary text, patches, changed-file content, command output, or failure data
that contains the exact credential. Non-success diagnostics remain sanitized.

Gateway mode preserves the previous behavior. OpenCode receives no provider
credential or credential header and calls a consumer-operated HTTPS gateway.
The gateway attaches any provider credential outside the Sandbox process.
Deterministic tests exercise Chat Completions and Responses without making a
live provider request. The Responses tests verify streaming text, native
function calls, analyzer StructuredOutput, complete local multi-turn tool
history, `store: false`, absence of `previous_response_id`, exact usage when
reported, unavailable usage when omitted, and sanitized HTTP or malformed-stream
failures. This proves the pinned OpenCode transport shape only. It does not
establish live compatibility with every Responses-like endpoint or model.

## Agent Sandbox protocol compatibility

GitHub Copilot Chat Completions and Ray Serve commonly use
`chat_completions`. OpenAI Responses and Copilot models that advertise only the
Responses endpoint use `responses`. Responses support does not imply universal
provider compatibility. The endpoint must implement the selected streaming
protocol, tool-call events, usage fields when claimed, and the model behavior
OpenCode expects. Deterministic fixtures are not a live provider smoke test.

## Agent Sandbox provider TLS

Every deployed Agent Sandbox provider endpoint must use HTTPS. Gateway mode
supports an internal service name whose CA is present in the immutable executor
image. The Fix runtime can also acknowledge a privately resolved public gateway
FQDN with a publicly trusted certificate by setting
`model_provider.public_ca_private_dns: true`. Direct mode must leave that field
false because it already identifies the actual provider endpoint.

The analyzer deny-by-default network policy supports external direct providers
only in Cilium mode, using an exact FQDN and port rule. Kubernetes NetworkPolicy
mode remains limited to internal provider Pods selected by namespace and Pod
labels. The Fix runtime still requires a separately reviewed consumer egress
policy.

## Independent critic gateway

The Agent Sandbox causal critic uses a stricter form of the model-gateway
boundary. It accepts only internal HTTPS service DNS, follows no redirects, and
sends no provider or gateway credential header. Model identity, provider
identity, token counts, and cost are recorded only when the gateway response
reports them.

The target gateway must authenticate and authorize the critic through an
infrastructure mechanism unavailable to the executor process, such as ambient
service-mesh workload identity. A required NetworkPolicy limits critic egress to
DNS and selected gateway pods, but that policy does not replace gateway-side
authorization.
