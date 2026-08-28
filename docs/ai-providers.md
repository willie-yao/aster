# AI providers

Aster supports OpenAI-compatible Chat Completions and Responses over HTTPS. The
selected endpoint and model must implement function calling. There is no default
provider and no tools-free fallback.

## Configure the API, endpoint, and model

Portable provider coordinates live under `ai` in `project.yaml`:

```yaml
ai:
  api: chat_completions       # or responses
  endpoint: https://provider.example/v1/chat/completions
  model: provider-model-id
  headers:                    # optional non-secret headers
    Some-Header: value
```

`endpoint` and `model` are required when AI is enabled. They may instead come
from `AI_ENDPOINT` and `AI_MODEL`. API, endpoint, and model resolution is
`project.yaml` first, then environment. See
[Project configuration](project-configuration.md) for the exact schema.

Set the bearer credential as `AI_TOKEN`. Chat Completions sends
`Authorization: Bearer <AI_TOKEN>` unless an explicit header overrides it.
Custom header values in `project.yaml` are literal, so never put a credential in
a public consumer file.

### GitHub Pages

The reusable workflow accepts provider coordinates from repository variables and
the token from a repository Secret:

```yaml
jobs:
  deploy:
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.10
    with:
      project-dir: .
      ai-api: ${{ vars.AI_API }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-model: ${{ vars.AI_MODEL }}
      ai-reasoning-effort: ${{ vars.AI_REASONING_EFFORT }}
    secrets:
      ai-token: ${{ secrets.AI_TOKEN }}
```

Variables keep private deployment coordinates out of committed source, but they
are not credentials. The Pages output always removes configured endpoint and
model fields before publication.

### Kubernetes

Set `ai.api`, `ai.endpoint`, `ai.model`, and optional `ai.reasoningEffort` in
Helm values. Put the token in an existing Secret and reference it with
`ai.existingSecret`. The chart and dashboard should not own the provider Secret
value. See [Kubernetes platform setup](kubernetes-platform.md#secret-ownership).

## Function calling is required

Chat Completions requests use `tools` and expect `tool_calls`. Responses requests
use function-call items and `function_call_output` continuations. An endpoint
that rejects tools produces an explicit unavailable analysis instead of a
weaker text-only fallback.

Provider compatibility is per endpoint and model. Confirm that the selected
model supports the configured API, tool calling, the required context size, and
streaming behavior when the endpoint uses streaming.

## Reasoning effort

Set provider reasoning effort through `AI_REASONING_EFFORT` or Helm
`ai.reasoningEffort`. Empty uses the provider default. The engine accepts:

```text
none, low, medium, high, xhigh, max
```

Unknown values fail before provider I/O. Chat Completions sends
`reasoning_effort`; Responses sends `reasoning.effort`. Provider and model
support varies. Aster does not infer effort from the model name or silently
change an unsupported value.

Reasoning effort is private content-free provenance. A non-empty value changes
provider and cache fingerprints, but existing reusable analyses remain cached
unless cache generation or another acceptance rule changes.

## Provider notes

### GitHub Copilot

Use the API listed by the model catalog's `supported_endpoints` field. Examples:

```yaml
ai:
  api: responses
  endpoint: https://api.githubcopilot.com/responses
  model: <responses-model>
```

```yaml
ai:
  api: chat_completions
  endpoint: https://api.githubcopilot.com/chat/completions
  model: <chat-completions-model>
```

`AI_TOKEN` is a PAT with the `copilot_chat` user permission. Model availability
and reasoning-effort support vary by subscription and change over time. Aster
adds `Copilot-Integration-Id: copilot-developer-cli` only for
`*.githubcopilot.com` endpoints, both for its own requests and for the sandbox
executors that run the coding agent.

### OpenAI

Responses:

```yaml
ai:
  api: responses
  endpoint: https://api.openai.com/v1/responses
  model: <model-id>
```

Chat Completions:

```yaml
ai:
  api: chat_completions
  endpoint: https://api.openai.com/v1/chat/completions
  model: <model-id>
```

`AI_TOKEN` is the API key. Responses requests use `store: false` and retain the
local conversation state needed for tool continuation.

### Azure OpenAI

Azure deployments commonly use a deployment-specific URL and an `api-key`
header. The stock Pages workflow supplies a bearer token, not a secret custom
header. Do not commit an Azure key in `project.yaml`. Use a trusted proxy that
translates the bearer credential, or a private deployment method that injects
the header without publishing it. The selected deployment must support function
calling.

### NVIDIA NIM, vLLM, Ollama, and compatible servers

Use the full OpenAI-compatible operation endpoint and the exact model identifier
reported by the server. Self-hosted endpoints may require a non-empty placeholder
`AI_TOKEN` even when the endpoint intentionally ignores authentication. Treat
that only as a deployment compatibility input, not as a security control.

Tool calling is model-template-specific. Configure the server's tool parser and
auto-tool selection when required. A compatible `/v1/models` response lets Aster
size context conservatively; otherwise use a provider configuration whose model
limits are known and tested.

### Ray Serve

Ray Serve's OpenAI-compatible LLM app works when the model ID matches
`model_loading_config.model_id` and the serving configuration enables automatic
tool choice with the correct parser. With the default route prefix, Chat
Completions is normally `/v1/chat/completions`.

## Cache provenance

Each analysis records content-free fingerprints for the endpoint, model, prompt,
and skills. Coordinate changes affect new analyses but do not invalidate an
otherwise reusable entry. Set `ai.cache_generation`, `AI_CACHE_GENERATION`, the
Pages `ai-cache-generation` input, or Helm `analysisCache.generation` for an
intentional reversible rebaseline.

Both provider APIs receive a content-free `prompt_cache_key` for analysis. Its
workspace component identifies the stable engine and consumer prompt prefix;
its shard identifies the exact enabled Tool schemas. Build IDs, source revisions,
artifact paths, prompts, and Tool contents are not included in the key.

## Usage metadata

The transports record provider-reported input, output, cached-input, cache-write,
and reasoning token fields when present. Missing metadata remains unavailable;
Aster does not estimate it from bytes or a tokenizer. Cost estimates use only
operator-configured pricing and are not provider invoices. Private usage ledgers
never include prompts, responses, endpoints, credentials, or repository content.

See [Agentic analysis](agentic.md#private-operational-data) and
[Server mode](server.md#private-data-boundary) for storage and access boundaries.

## Agent Sandbox provider compatibility

Agent Sandbox uses a separate version-pinned OpenCode transport, but the provider
contract is shared by Fix generation and the analysis shadow.

| `model_provider.api` | OpenCode transport | Supported authentication |
| --- | --- | --- |
| `chat_completions` | `@ai-sdk/openai-compatible` | Direct bearer, direct unauthenticated, or explicit tokenless gateway. |
| `responses` | `@ai-sdk/openai` | Direct bearer only with pinned OpenCode 1.18.2. |

Configure the complete HTTPS operation endpoint. Chat Completions must end in
`/chat/completions`; Responses must end in `/responses`. Aster rejects an API and
path mismatch instead of guessing a provider base URL.

Direct bearer mode exposes one dedicated inference credential through the fixed
executor environment. Direct unauthenticated mode renders no Secret reference.
Gateway mode is tokenless, requires `auth.type: none`, and depends on gateway-side
workload authorization that the executor cannot impersonate. Network reachability
alone is not authentication.

Pinned OpenCode 1.18.2 supports empty provider-default effort plus `none`, `low`,
`medium`, `high`, and `xhigh`. It rejects `max` even when the engine-native
provider transport supports that value. The project and Helm reasoning-effort
values must match.

Every deployed endpoint must use HTTPS. The Fix-only
`public_ca_private_dns` or Helm `publicCAPrivateDNS` field must remain false for
direct providers and may be true only for a privately resolved public gateway
FQDN whose certificate chains to a public CA. The analysis shadow has no
equivalent field.

RuntimeClass, gateway authorization, egress, and Secret ownership remain
platform-owned and are documented in
[Kubernetes platform setup](kubernetes-platform.md). The optional ConfigMap CA
bundle is a Fix-only contract documented in the
[Kubernetes operator reference](kubernetes-reference.md#agent-sandbox-fix-runtime);
it does not extend the analysis shadow. Analysis-shadow private CA trust must be
present in the immutable executor image.

The named bearer Secret must already exist in the execution namespace. Aster and
the chart reference only its exact name and key; they do not create, copy, read,
or print the value. Use a dedicated inference-only credential, never
`BOT_TOKEN`, `ISSUE_TOKEN`, OAuth credentials, GitHub read credentials, or a general
PAT. Admission rejects extra credential and environment shapes, and the executor
rejects exact credential leakage from outputs and results.

Focused runtime guides:

- Fix workflow and project example: [Fix PR generation](fix-prs.md#agent-sandbox-opencode-executor)
- Analysis-shadow authority and evaluation: [Agent Sandbox OpenCode analyzer](maintainer/agent-sandbox-opencode-analyzer.md)
- Exact project fields: [Project configuration](project-configuration.md#experimental-agent-sandbox-fix-configuration)
- RuntimeClass, Secret, CA, and egress ownership: [Kubernetes platform setup](kubernetes-platform.md)
