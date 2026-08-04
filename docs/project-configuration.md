# Project configuration

Each consumer owns one strict `project.yaml`. Unknown fields are errors. Start
with [`configs/example/project.yaml`](../configs/example/project.yaml) or let
[`fetcher onboard`](onboarding-a-new-project.md) generate it. Add optional
sections only when a working deployment needs them. The guided onboarding
wizard shows the source and confidence for inferred identity and TestGrid values,
then lets you edit them before any file is written.

Use `fetcher onboard doctor -project-dir <dir>` to run the same strict parser on
an existing consumer before deployment.

## Configuration boundaries

`project.yaml` owns portable project behavior and analysis policy. Workflow
inputs and Helm values own infrastructure, credentials, and execution tuning.

The consumer files have different owners:

| File | Owns |
| --- | --- |
| `project.yaml` | Portable project identity, discovery, storage, branding, analysis policy, and optional features |
| `prompts/system.md` | Project-specific architecture and failure knowledge |
| `skills/*.yaml` | Optional portable diagnostic recipes and evidence requirements |
| Pages workflow or Helm values | Infrastructure, credentials, runner or cluster settings, and Orka execution settings |

Do not copy Helm or workflow tuning into `project.yaml`. Do not put project
identity or artifact routing into Helm values.

Failure-analysis placement follows the same boundary. The default in-process
runtime and the experimental Helm `analysisRuntime.type: orka-container`
selector are deployment infrastructure. Do not add an analysis runtime field to
`project.yaml`. The `ai:` block continues to own analysis policy for both
runtimes.

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
| `branding` | Site identity, URL paths, and default repository |

`short_name` is an optional compact display label. The wizard suggests one only
when the repository name provides a reasonable abbreviation. Type `none` in the
wizard to omit the suggestion. Inferred category tokens are also editable and
can be cleared with the same sentinel.

For Pages, set
`branding.base_path` to `/<host-repo>` and `site_url` to the full Pages URL. For
Kubernetes, use `/` and the ingress URL.

## Analysis source repository

Analysis can read a repository that differs from branding and write targets:

```yaml
ai:
  source_repo:
    owner: kubernetes-sigs
    name: cluster-api-provider-azure
```

Omit `ai.source_repo` to use `branding.source_repo`. This setting controls
read-only analysis grounding only. `issues.repo` and `ai.fix_prs.repo` remain
independent write targets and continue to default to `branding.source_repo`.
Both `owner` and `name` are required when `ai.source_repo` is present.

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

Provider coordinates can come from YAML:

```yaml
ai:
  endpoint: "https://api.example.net/v1/chat/completions"
  model: "model-id"
  cache_generation: ""
```

Public consumers normally omit provider values and use `AI_ENDPOINT`, `AI_MODEL`,
and `AI_TOKEN` from the deployment. For cache generation, a non-empty
`AI_CACHE_GENERATION` overrides `ai.cache_generation`; empty preserves the
historical cache-key shape. Generation values are limited to 64 characters and
may contain alphanumerics, dot, underscore, and hyphen.
The experimental Helm `orka-container` runtime is the exception: its API mode,
endpoint, and model come from Helm `ai.api`, `ai.endpoint`, and `ai.model` so the
fetcher pattern pass and analyzer Tasks use the same deployment coordinates.

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
    max_retries: 0
```

`critique.max_retries: 0` publishes and caches the model's original answer when
the investigation floors pass, while retaining critique telemetry. Positive
values enable bounded repair and require critique success for cache reuse.

Do not commit credentials under `ai.headers`. `AI_TOKEN` is the supported bearer
token channel. Use a trusted proxy or custom deployment for providers that need
a secret in another header.

### AI usage accounting

Private token accounting is enabled by default when the `ai:` block is present.
Configure retention and optional cost estimates under `ai.usage`:

```yaml
ai:
  usage:
    enabled: true
    retention_days: 90
    recent_operations: 250
    pricing:
      currency: USD
      input_per_million: "1.25"
      cached_input_per_million: "0.125"
      output_per_million: "10"
```

`retention_days` defaults to `90` and accepts `1` through `3650` when set.
`recent_operations` defaults to `250`, accepts `0` through `5000`, and controls
the detailed private drill-down list. Set it to `0` to retain daily aggregates
without recent operation records. Set `enabled: false` to disable both token and
cost accounting.

Pricing is optional. Without it, the dashboard records provider-reported tokens
but does not assign a cost. `currency` must contain exactly three ASCII uppercase
letters. Rates are decimal currency units per one million tokens. Omit
`cached_input_per_million` to price cached input at the regular input rate. Rates
must be non-negative decimal strings no greater than `1000000`.

Cost values are estimates, not provider invoices. Providers may omit usage or
apply discounts, retries, minimum charges, or non-token fees that are not present
in the model response. Usage files are private operational state and are removed
from Pages artifacts. A currency change is rejected while retained nonzero cost
estimates still use the previous currency.

## Custom skills

Diagnostic recipes live under `skills/*.yaml` or `skills/*.yml`. Their presence
is the opt-in. Pages, local development, and the Kubernetes bundle wrapper load
the same directory. Filenames must be valid ConfigMap keys and cannot be
`project.yaml`. The analyzer loads these recipes, enforces their
required evidence, and includes the skill-set hash in cache acceptance. See
[Custom diagnostic skills](skills.md).

Deployments that require a consumer bundle can fail startup when it is absent
or too small:

```yaml
ai:
  consumer_skills:
    required: true
    minimum_count: 11
```

`required: true` without `minimum_count` requires at least one consumer recipe.
Errors report counts only and do not expose recipe contents.

## Optional features

Keep these sections out of the first-run config. Add them after the dashboard
publishes the expected jobs:

- `notifications.email`: [Email notifications](notifications.md)
- `issues`: [GitHub issues](github-issues.md)
- `ai.fix_prs`: [Agent-proposed fix PRs](fix-prs.md)
- `ai.source_investigation`: optional read-only Orka source runtime for analysis chat

These features require deployment secrets and, in some cases, additional writer
runtime dependencies. Their focused guides contain complete examples.

For Orka fix generation, `agent_runtime.type: orka` selects the dashboard
backend. The operator-managed Orka Agent selects its CLI with
`spec.runtime.type: opencode` and owns the model endpoint, model ID, and model
Secret. Keep those settings out of `project.yaml`.
Helm deployments must also configure the matching
`orka.fixRuntime.admission` contract. The duplicate values are intentional: the
Kubernetes API must know the exact Agent, public repository, turns, Bash policy,
timeout, and retries before it admits a Task. Guarded fix Tasks do not accept
`git_secret`; private repository generation is unavailable until Orka exposes a
pinnable credential binding.

Source investigation is a separate read-only contract and does not inherit
`ai.fix_prs`. Configure it only for Kubernetes-native analysis chat:

```yaml
ai:
  source_investigation:
    agent_ref: guarded-source-reader
    api: http://orka.orka-system.svc.cluster.local:8080
    namespace: orka-system
    git_secret: source-repo-readonly
    version: v1
    retries: 1
    max_turns: 30
    timeout: 10m
```

`agent_ref` and `api` are required when the block is present. `timeout` must be
positive and at most 30 minutes. `retries` must be `0` through `2`. A nonzero
`max_turns` must be `1` through `1000`; zero uses the default. `git_secret`
belongs to Orka and must provide read-only clone credentials. Provider and model
credentials remain owned by the Agent. The Agent runtime must support Orka's
enforced `orka.ai/agent-read-only`
contract; an Orka release that rejects guarded OpenCode cannot use OpenCode here.
The dashboard uses the effective `ai.source_repo` and the selected build's exact
`repo_refs` commit, so there is no repository override or branch fallback. Bare
full SHAs and unambiguous `ref:fullSHA` values are supported. Composite
presubmit refs are rejected rather than guessing which commit was tested.

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
