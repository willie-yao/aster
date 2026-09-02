# AI providers

Aster supports OpenAI-compatible Chat Completions and Responses over HTTPS. The selected endpoint and model must implement function calling. There is no default provider and no tools-free fallback.

## Configure provider coordinates and project behavior

API, endpoint, model, and cache generation are deployment-owned. Direct commands read `AI_API`, `AI_ENDPOINT`, `AI_MODEL`, `AI_CACHE_GENERATION`, and `AI_TOKEN`. `AI_API` defaults to `chat_completions`; endpoint and model are required when AI is enabled. Do not put `api`, `endpoint`, `model`, or `cache_generation` under `ai` in `project.yaml`; strict decoding rejects them.

Project-owned provider behavior and analysis policy remain under `ai` in `project.yaml`. This includes `headers`, `service_tier`, `agentic`, `critique`, and `usage`:

```yaml
ai:
  service_tier: flex            # optional, Responses on api.openai.com only
  headers:                      # optional non-secret headers
    Some-Header: value
  max_iters: 15                  # agentic loop tuning is inlined under ai
  critique:
    max_retries: 0
  usage:
    enabled: true
```

Set the bearer credential as `AI_TOKEN`. Chat Completions sends `Authorization: Bearer <AI_TOKEN>` unless an explicit header overrides it. Custom header values in `project.yaml` are literal, so never put a credential in a public consumer file. See [Project configuration](project-configuration.md) for the exact project-owned schema.

### GitHub Pages

The reusable workflow accepts provider coordinates from repository variables and the token from a repository Secret:

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
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

Variables keep private deployment coordinates out of committed source, but they are not credentials. The Pages output always removes configured endpoint and model fields before publication.

### Kubernetes

Set `ai.api`, `ai.endpoint`, `ai.model`, and optional `ai.reasoningEffort` in Helm values. Put the token in an existing Secret and reference it with `ai.existingSecret`. The chart and dashboard should not own the provider Secret value. See [Kubernetes platform setup](kubernetes-platform.md#secret-ownership).

## Function calling is required

Chat Completions requests use `tools` and expect `tool_calls`. Responses requests use function-call items and `function_call_output` continuations. An endpoint that rejects tools produces an explicit unavailable analysis instead of a weaker text-only fallback.

Provider compatibility is per endpoint and model. Confirm that the selected model supports the configured API, tool calling, the required context size, and streaming behavior when the endpoint uses streaming.

## Reasoning effort

Set provider reasoning effort through `AI_REASONING_EFFORT` or Helm `ai.reasoningEffort`. Empty uses the provider default. The engine accepts:

```text
none, low, medium, high, xhigh, max
```

Unknown values fail before provider I/O. Chat Completions sends `reasoning_effort`; Responses sends `reasoning.effort`. GPT-5.4 and later OpenAI models support Chat Completions tool calling only with `none`, so Aster rejects another explicit effort for canonical `gpt-5.4`-and-later model IDs and provider-qualified IDs ending in that canonical form. Use Responses to request higher effort. Arbitrary provider aliases cannot be inferred; other provider and model support varies. Aster does not silently change an unsupported value.

Reasoning effort is private content-free provenance. A non-empty value changes provider and cache fingerprints, but existing reusable analyses remain cached unless cache generation or another acceptance rule changes.

## Provider notes

### GitHub Copilot

Use the API listed by the model catalog's `supported_endpoints` field.

Direct-command deployment variables for a Responses model:

```bash
export AI_API=responses
export AI_ENDPOINT=https://api.githubcopilot.com/responses
export AI_MODEL=<responses-model>
```

Direct-command deployment variables for a Chat Completions model:

```bash
export AI_API=chat_completions
export AI_ENDPOINT=https://api.githubcopilot.com/chat/completions
export AI_MODEL=<chat-completions-model>
```

`AI_TOKEN` is a PAT with the `copilot_chat` user permission. Model availability and reasoning-effort support vary by subscription and change over time. Aster adds `Copilot-Integration-Id: copilot-developer-cli` only for `*.githubcopilot.com` endpoints, both for its own requests and for the sandbox executors that run the coding agent.

### OpenAI

Responses direct-command deployment variables:

```bash
export AI_API=responses
export AI_ENDPOINT=https://api.openai.com/v1/responses
export AI_MODEL=<model-id>
```

Optional project-owned behavior in `project.yaml`:

```yaml
ai:
  service_tier: flex
```

Flex processing is accepted only for this exact OpenAI host and the Responses API. Aster uses a minimum 15-minute analysis timeout, retries provider capacity responses with backoff, then sends the final attempt with `service_tier: auto`. The provider-echoed tier is recorded on each model-request trace. Aster also preserves the Responses assistant `phase` field when compaction or forced finalization reconstructs an assistant message for a later request.

Chat Completions direct-command deployment variables:

```bash
export AI_API=chat_completions
export AI_ENDPOINT=https://api.openai.com/v1/chat/completions
export AI_MODEL=<model-id>
```

`AI_TOKEN` is the API key. Responses requests use `store: false` and retain the local conversation state needed for tool continuation.

### Azure OpenAI

Azure deployments commonly use a deployment-specific URL and an `api-key` header. The stock Pages workflow supplies a bearer token, not a secret custom header. Do not commit an Azure key in `project.yaml`. Use a trusted proxy that translates the bearer credential, or a private deployment method that injects the header without publishing it. The selected deployment must support function calling.

### NVIDIA NIM, vLLM, Ollama, and compatible servers

Use the full OpenAI-compatible operation endpoint and the exact model identifier reported by the server. Self-hosted endpoints may require a non-empty placeholder `AI_TOKEN` even when the endpoint intentionally ignores authentication. Treat that only as a deployment compatibility input, not as a security control.

Tool calling is model-template-specific. Configure the server's tool parser and auto-tool selection when required. A compatible `/v1/models` response lets Aster size context conservatively; otherwise use a provider configuration whose model limits are known and tested.

### Ray Serve

Ray Serve's OpenAI-compatible LLM app works when the model ID matches `model_loading_config.model_id` and the serving configuration enables automatic tool choice with the correct parser. With the default route prefix, Chat Completions is normally `/v1/chat/completions`.

## Cache provenance

Each analysis records content-free fingerprints for the endpoint, model, prompt, and skills. Coordinate changes affect new analyses but do not invalidate an otherwise reusable entry. Set `AI_CACHE_GENERATION`, the Pages `ai-cache-generation` input, or Helm `analysisCache.generation` for an intentional reversible rebaseline.

Both provider APIs receive a content-free `prompt_cache_key` on the main analysis loop and Tool-backed critique repairs. Its workspace component routes the stable engine and consumer prompt prefix together; its shard identifies the exact enabled Tool schemas. Build IDs, source revisions, artifact paths, prompts, and Tool contents are not included in the key.

## Usage metadata

The transports record provider-reported input, output, cached-input, cache-write, and reasoning token fields when present. Missing metadata remains unavailable; Aster does not estimate it from bytes or a tokenizer. Cost estimates use only operator-configured pricing and are not provider invoices. Private usage ledgers never include prompts, responses, endpoints, credentials, or repository content.

See [Agentic analysis](agentic.md#private-operational-data) and [Server mode](server.md#private-data-boundary) for storage and access boundaries.

## Agent Sandbox provider compatibility

Agent Sandbox Fix generation uses a separate version-pinned OpenCode transport.

| `model_provider.api` | OpenCode transport | Supported authentication |
| --- | --- | --- |
| `chat_completions` | `@ai-sdk/openai-compatible` | Direct bearer, direct unauthenticated, or explicit tokenless gateway. |
| `responses` | `@ai-sdk/openai` | Direct bearer only with pinned OpenCode 1.18.2. |

Configure the complete HTTPS operation endpoint. Chat Completions must end in `/chat/completions`; Responses must end in `/responses`. Aster rejects an API and path mismatch instead of guessing a provider base URL.

Direct bearer mode exposes one dedicated inference credential through the fixed executor environment. Direct unauthenticated mode renders no Secret reference. Gateway mode is tokenless, requires `auth.type: none`, and depends on gateway-side workload authorization that the executor cannot impersonate. Network reachability alone is not authentication.

Pinned OpenCode 1.18.2 supports empty provider-default effort plus `none`, `low`, `medium`, `high`, and `xhigh`. It rejects `max` even when the engine-native provider transport supports that value. The project and Helm reasoning-effort values must match.

Every deployed endpoint must use HTTPS. The `public_ca_private_dns` or Helm `publicCAPrivateDNS` field must remain false for direct providers and may be true only for a privately resolved public gateway FQDN whose certificate chains to a public CA.

RuntimeClass, gateway authorization, egress, and Secret ownership remain platform-owned and are documented in [Kubernetes platform setup](kubernetes-platform.md). The optional ConfigMap CA bundle is documented in the [Kubernetes operator reference](kubernetes-reference.md#agent-sandbox-fix-runtime).

The named bearer Secret must already exist in the execution namespace. Aster and the chart reference only its exact name and key; they do not create, copy, read, or print the value. Use a dedicated inference-only credential, never `BOT_TOKEN`, `ISSUE_TOKEN`, OAuth credentials, GitHub read credentials, or a general PAT. Admission rejects extra credential and environment shapes, and the executor rejects exact credential leakage from outputs and results.

Focused runtime guides:

- Fix workflow and project example: [Fix PR generation](fix-prs.md#agent-sandbox-opencode-executor)
- Exact project fields: [Project configuration](project-configuration.md#experimental-agent-sandbox-fix-configuration)
- RuntimeClass, Secret, CA, and egress ownership: [Kubernetes platform setup](kubernetes-platform.md)
