# Project configuration

Each consumer owns one strict `project.yaml`. Unknown fields are errors. Start
with [`configs/example/project.yaml`](../configs/example/project.yaml) or let
[`fetcher onboard`](onboarding-a-new-project.md) generate it. Add optional
sections only when a working deployment needs them.

## Configuration boundaries

Three files have different owners:

| File | Owns |
| --- | --- |
| `project.yaml` | Portable project identity, discovery, storage, branding, analysis policy, and optional features |
| `prompts/system.md` | Project-specific architecture and failure knowledge |
| Pages workflow or Helm values | Infrastructure, credentials, runner or cluster settings, and Orka execution settings |

Do not copy Helm or workflow tuning into `project.yaml`. Do not put project
identity or artifact routing into Helm values.

## Minimal configuration

```yaml
id: myproject
name: "My Project"

testgrid:
  dashboard: "sig-myproject-periodics"

storage:
  provider: gcs
  bucket: kubernetes-ci-logs

branding:
  title: "My Project Prow Dashboard"
  base_path: "/myproject-dashboard"
  site_url: "https://my-org.github.io/myproject-dashboard"
  source_repo:
    owner: my-org
    name: myproject
```

| Field | Purpose |
| --- | --- |
| `id` | Stable lowercase identifier used in task identities, cache keys, and logs |
| `name` | Human-readable project name |
| `testgrid.dashboard` | TestGrid annotation used by the default discovery source |
| `storage` | Artifact backend and bucket |
| `branding` | Site identity, URL paths, and source repository |

`short_name` is an optional compact display label. For Pages, set
`branding.base_path` to `/<host-repo>` and `site_url` to the full Pages URL. For
Kubernetes, use `/` and the ingress URL.

## Storage

```yaml
# Native Google Cloud Storage.
storage:
  provider: gcs
  bucket: kubernetes-ci-logs

# A gcsweb gateway, including S3-backed Prow installations.
storage:
  provider: gcsweb
  bucket: my-prow
  base: "https://gcsweb.example.net/s3"
  prow_base: "https://prow.example.net/view/s3"

# A downloaded artifact tree for tests or offline runs.
storage:
  provider: local
  base: "/absolute/path/to/artifacts"
  web_base: "https://artifacts.example.net"
```

`web_base` overrides artifact links. `prow_base` overrides Prow build links. The
local provider is intended for tests and offline fetches.

## Job discovery

The default source reads Kubernetes test-infra job configuration and keeps jobs
whose `testgrid-dashboards` annotation contains `testgrid.dashboard`.

For another Prow installation, discover directly from its artifact bucket:

```yaml
discovery:
  source: bucket
  job_filters: ["integration-"]
```

Omit `job_filters` to include every job in the bucket.

Periodics are included by default. Add presubmits with:

```yaml
source:
  include_presubmits: true
```

## Categories

Categories are optional. Without them, the landing page renders one flat job
grid. Rules use case-insensitive substring matching and the first match wins.

```yaml
categories:
  - match: "conformance"
    id: conformance
    label: Conformance
  - match: "e2e"
    id: e2e
    label: E2E

category_display_order: [e2e, conformance, other]
```

Unmatched jobs use the reserved `other` category.

## Analysis configuration

AI is optional at the fetcher level. When enabled, it needs a token, a non-empty
`prompts/system.md`, and a function-calling model.

For in-process analysis, provider coordinates can come from YAML:

```yaml
ai:
  endpoint: "https://api.example.net/v1/chat/completions"
  model: "model-id"
```

Public consumers normally omit those values and use `AI_ENDPOINT`, `AI_MODEL`,
and `AI_TOKEN` from the deployment. YAML wins when both are set.

Most projects do not need analysis tuning. The defaults are designed to work
without an `ai:` block. Add only the setting that a measured model or artifact
constraint requires:

```yaml
ai:
  tools: [filesystem, k8s]
  concurrency: 1
  max_iters: 15
  timeout: 5m
  min_tool_calls: 2
  min_gcs_bytes: 0
  single_tool_call: false
  critique:
    max_retries: 2
```

Do not commit credentials under `ai.headers`. `AI_TOKEN` is the supported bearer
token channel. Use a trusted proxy or custom deployment for providers that need
a secret in another header.

### In-process and Orka compatibility

The Kubernetes skeleton fetch still uses discovery, storage, branding, and
categories before Orka begins. The Orka ingestor runs configured notifications
and GitHub reconciliation after analysis completes.

| Configuration | In-process | Orka preview |
| --- | --- | --- |
| `id`, `name`, `short_name` | Yes | Yes |
| `source`, `testgrid`, `discovery` | Yes | Yes |
| `storage.*` | Yes | Yes |
| `branding`, categories | Yes | Yes |
| `prompts/system.md` | Yes | Yes |
| `skills/*.yaml` | Yes | Yes |
| `ai.tools` | Yes | Yes |
| `ai.min_tool_calls` | Yes | Yes |
| `ai.min_gcs_bytes` | Yes | Yes |
| `ai.endpoint`, `ai.model`, `ai.headers` for per-failure analysis | Yes | No, use an Orka Provider and `orka.model` |
| `ai.concurrency`, `max_iters`, `timeout` | Yes | No, use `orka.*` execution settings where applicable |
| `ai.single_tool_call`, `ai.critique.*` | Yes | No, the compatibility worker and validation tools own convergence and acceptance |
| `notifications`, `issues`, `ai.fix_prs` | Yes | Yes, after Orka ingestion |

Provider values under `ai.*` may still be used by optional post-analysis AI
review, but they do not configure Orka's per-failure Tasks.

Orka is a strategic backend that is still in preview. Its current Provider,
model, task timeout, retry, placement, and load controls live in Helm `orka.*`
values. See the [Orka quickstart](../experimental/orka/QUICKSTART.md).

## Custom skills

Diagnostic recipes live under `skills/*.yaml`. Their presence is the opt-in.
Both analysis backends load the same recipes, enforce their required evidence,
and include the skill-set hash in cache or Task identity. See
[Custom diagnostic skills](skills.md).

## Optional features

Keep these sections out of the first-run config. Add them after the dashboard
publishes the expected jobs:

- `notifications.email`: [Email notifications](notifications.md)
- `issues`: [GitHub issues](github-issues.md)
- `ai.fix_prs`: [Agent-proposed fix PRs](fix-prs.md)

These features require deployment secrets and, in some cases, additional writer
runtime dependencies. Their focused guides contain complete examples.

## Validate a config

A one-build, discovery-only fetch validates the strict schema without making AI
calls:

```bash
./bin/fetcher -project-dir=../my-consumer -ai=false -builds=1
```

Then inspect the job count:

```bash
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']))"
```
