# Project configuration

Each consumer owns one strict `project.yaml`. Unknown fields are errors. Start
with [`configs/example/project.yaml`](../configs/example/project.yaml) or let
[`aster onboard`](onboarding-a-new-project.md) generate it. Add optional
sections only when a working deployment needs them. The guided onboarding
wizard shows the source and confidence for inferred identity and TestGrid values,
then lets you edit them before any file is written.

Use `aster onboard doctor -project-dir <dir>` to run the same strict parser on
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
| Pages workflow or Helm values | Infrastructure, credentials, runner or cluster settings, and runtime placement |

Do not copy Helm or workflow tuning into `project.yaml`. Do not put project
identity or artifact routing into Helm values.

Failure analysis always runs in-process next to the fetcher or worker. Do not
add an analysis runtime field to `project.yaml`; the `ai:` block owns analysis
policy.

## Minimal configuration

```yaml
id: myproject
name: "My Project"

discovery:
  testgrid_dashboard: "sig-myproject-periodics"

storage:
  bucket: kubernetes-ci-logs

branding:
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
| `discovery.testgrid_dashboard` | TestGrid annotation used by the default discovery source |
| `storage` | Artifact bucket. The provider defaults to `gcs`. |
| `branding` | Site URL paths and default repository. The title defaults to `<name> Prow Dashboard`. |

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
    owner: example-org
    name: source-project
```

Omit `ai.source_repo` to use `branding.source_repo`. This setting controls
read-only analysis grounding only. `issues.repo` and `ai.fix_prs.repo` remain
independent write targets and continue to default to `branding.source_repo`.
Both `owner` and `name` are required when `ai.source_repo` is present.

## Storage

```yaml
# Native Google Cloud Storage. `gcs` is the default provider.
storage:
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
whose `testgrid-dashboards` annotation contains `discovery.testgrid_dashboard`.

For a reproducible evaluation or incident replay, optionally pin discovery to an
exact lowercase 40-character `kubernetes/test-infra` commit:

```yaml
discovery:
  source: testgrid
  test_infra_revision: 2e1a38bcca26b0fe1a46e7b2bf652e1de49cacca
```

When omitted, each fetch resolves the current test-infra `master` commit and
uses that one snapshot consistently for the run. A configured pin never falls
back to `master`. The effective revision is published as
`discovery.resolved_test_infra_revision` in `manifest.json`; the Pages workflow
also copies it to `provenance.json`. The field is invalid with bucket discovery.

For another Prow installation, discover directly from its artifact bucket:

```yaml
discovery:
  source: bucket
  job_filters: ["integration-"]
```

Omit `job_filters` to include every job in the bucket.

For a narrow dashboard or evaluation, list exact periodic or postsubmit job
names. Exact discovery validates each job through its direct bucket index and
does not enumerate the bucket root:

```yaml
discovery:
  source: bucket
  exact_jobs:
    - periodic-project-e2e
    - periodic-project-upgrade
```

`exact_jobs` and `job_filters` are mutually exclusive. Exact names are
case-sensitive and a missing name fails discovery instead of silently publishing
a partial dashboard. When presubmits are enabled, an exact name may also resolve
through its direct `pr-logs/directory/<job>/` index.

Periodics are included by default. Add presubmits with:

```yaml
discovery:
  include_presubmits: true
```

## Pull request triage

Pull request triage is optional and targets the open pull requests of
`branding.source_repo`.

```yaml
pull_requests:
  enabled: true
  max: 100
  builds_per_job: 3
  comment:
    enabled: true
    dry_run: true
    max_per_pass: 10
```

| Field | Default | Purpose |
| --- | ---: | --- |
| `pull_requests.enabled` | `false` | Publish the pull request index, detail pages, and shared-failure view. |
| `pull_requests.max` | `100` | Bound open pull requests per pass, most recently updated first. |
| `pull_requests.builds_per_job` | `3` | Bound builds inspected per presubmit before selecting the newest applicable build. |
| `pull_requests.comment.enabled` | `false` | Enable the GitHub App commenting pass. Requires triage. |
| `pull_requests.comment.dry_run` | `true` | Log comment bodies without posting unless explicitly set to `false`. |
| `pull_requests.comment.max_per_pass` | `10` | Bound comments posted in one pass. |

GitHub credentials, attribution semantics, shared failures, bot-comment safety,
and server-side AI escalation are documented in
[Pull request triage](pull-request-triage.md). `discovery.include_presubmits` is
not required for triage and changes only whether presubmit jobs appear in the
main job dashboard.

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

## Attention thresholds

`attention` is optional and tunes which failing tests the dashboard surfaces.
Omitting it keeps the engine defaults, and every field below is independent.

```yaml
attention:
  # Consecutive failures required for a `persistent` classification. Default 3.
  persistent_after: 3
  # Omit the whole block to leave the pass-rate rule off.
  low_pass_rate:
    threshold: 1.0    # required, 0 to 1
    min_runs: 5       # default 5
    recent_runs: 0    # 0 or omitted measures the full fetch window
    max_items: 50     # default 50
```

### `persistent_after`

This is a **classification** knob, not a display filter. Changing it moves
every consumer of the classification at once:

- a test's published `classification` (`persistent`, `flaky`, `one-off`)
- the `persistent_failures` and `most_flaky` sections of `flakiness.json`
- the `known_flake` verdict in pull request attribution, which reads the
  base-branch flakiness history
- email notification eligibility, which follows `persistent_failures`
- which findings issue recovery treats as still active, which also follows
  `persistent_failures`
- the failing-versus-flaky styling in the UI

Lowering it to `1` classifies a single failure as persistent, which is rarely
what a project wants. Reach for `low_pass_rate` instead when the goal is to
*see* more tests rather than to redefine what "persistent" means.

### `low_pass_rate`

This is a **selection** rule. It publishes a `low_pass_rate` section in
`flakiness.json` that the Needs Attention band renders as its own group, and it
never changes a test's `classification`.

A test is selected when its pass rate over the window is strictly below
`threshold`. So `threshold: 1.0` surfaces any test that failed at least once,
and `threshold: 0` surfaces nothing.

- `min_runs` is the evidence guard. One failure out of two runs is a 50% pass
  rate but weak signal, so a test with fewer runs than this in the window is
  never selected.
- `recent_runs` narrows the measurement to the newest N runs of each test, so a
  failure the test has since recovered from drops out. The guard applies to the
  same narrowed window, which means `recent_runs` below `min_runs` selects
  nothing. Each entry publishes its own `window_runs` and `pass_rate` because a
  narrowed window disagrees with the entry's whole-window `fail_rate`.
- `max_items` caps the published section. The overview renders at most ten test
  alerts across all groups combined, and the pass-rate group only fills the
  budget the recent, persistent, and flaky groups leave behind, so a large
  dashboard cannot be flooded by this rule alone.

`threshold: 1.0` is a reasonable setting for a project with mostly-green
periodics that treats any failure as worth a look. Start with the default
`min_runs` and tighten from there if the group stays noisy.

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
optional `AI_REASONING_EFFORT`, and `AI_TOKEN` from the deployment. For cache generation, a non-empty
`AI_CACHE_GENERATION` overrides `ai.cache_generation`; empty preserves the
historical cache-key shape. Generation values are limited to 64 characters and
may contain alphanumerics, dot, underscore, and hyphen.
`ai.service_tier: flex` enables OpenAI Flex processing only when `ai.api` is
`responses` and the endpoint host is exactly `api.openai.com`. Other providers
are rejected before any request. Flex raises an effective timeout below 15
minutes to 15 minutes and falls back to `auto` after repeated provider capacity
responses.
Most projects do not need analysis tuning. The defaults are designed to work
without an `ai:` block. Add only the setting that a measured model or artifact
constraint requires:

For example, a non-Kubernetes project can remove the Kubernetes tool group:

```yaml
ai:
  tools: [filesystem]
```

The engine defaults to 15 iterations, a 5-minute per-failure timeout, two tool
calls, no byte floor, parallel tool calls, zero critique repair requests, the
`hard` critique cache policy. Override one of those defaults only after a measured provider or artifact constraint requires
it.

`critique.max_retries` controls provider repair attempts only. `0` evaluates
critique without making a critique repair request. `critique.cache_policy`
independently controls cache reuse. The two settings are unrelated: changing
`max_retries` never changes which findings block reuse.

- `strict`: actionable hard failures and soft warnings block reuse.
- `hard`: only hard safety, grounding, and correctness failures block reuse.
- `advisory`: critique findings never block reuse.

If `cache_policy` is omitted it defaults to `hard`, whatever `max_retries` is.
Evidence that is deterministically unavailable remains a warning under every
policy. Structural validation, publication sanitization, and critique-version
validation remain mandatory.

Publication disposition is separate from cache policy. A draft whose causal claim
has no validated artifact citation is published as `preliminary` with an
`artifact_grounding_incomplete` warning under every policy. A
`citations_verified` disposition means retained citations were read and their
quotes occur at the stated artifact ranges. It does not mean the cited text
entails the cause or supports every causal link. Preliminary publication does not
directly authorize an action. A diagnosis that passes the
separate usable-diagnosis check may still feed correlation or chat-derived Fix
flows that apply their own evidence gates. Cache policy can still change whether the selected draft is reusable, while
deterministic draft selection decides which parseable candidate is published.

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
      cache_write_input_per_million: "1.50"
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
`cache_write_input_per_million` is optional and has no inferred default. The
dashboard charges that rate only when a recognized provider response reports
cache-creation input tokens. When the provider omits cache-write counts, the
report marks cache-write coverage missing rather than estimating from total
input tokens.

Cost values are estimates, not provider invoices. Providers may omit usage or
apply discounts, retries, minimum charges, or non-token fees that are not present
in the model response. Usage files are private operational state and are removed
from Pages artifacts. A currency change is rejected while retained nonzero cost
estimates still use the previous currency.

Ledger version 2 adds cache-write counts, coverage provenance, and model
breakdowns while loading version 1 files without dropping their token or cost
totals. Legacy days remain explicitly coverage-unknown because cache-write and
historical model counts cannot be reconstructed. Model identifiers are stored
only when they pass a bounded safe identifier check. Endpoints and credentials
are never persisted.

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

Keep optional sections out of the first-run config. Add them only after the
dashboard publishes the expected jobs and analyses.

- `notifications.email`: [Email notifications](notifications.md)
- `issues`: [GitHub issues](github-issues.md)
- `ai.fix_prs`: [Experimental Fix PR generation](fix-prs.md)

Authenticated chat, File Issue, and Mark Resolved are server deployment features,
not separate analysis runtimes. They do not require a Fix PR runtime.
See the [documentation feature map](README.md#optional-features) for deployment
requirements and the recommended enablement order.

The focused feature guides own their credential, admission, timeout, and
security contracts. Avoid duplicating those settings into a first-run
`project.yaml`.

## Experimental Agent Sandbox Fix configuration

`ai.fix_prs.agent_runtime` configures the non-secret, portable part of the
Agent Sandbox Fix executor. Agent Sandbox is disabled by default and is the only
supported Fix runtime.

| Field | Contract |
| --- | --- |
| `type` | Optional. Defaults to and accepts only `agent-sandbox`. |
| `max_turns` | Defaults to `30`; must not exceed `1000`. |
| `allow_bash` | Defaults to `false` and must remain false. |
| `timeout` | Defaults to `10m`; must be positive and at most `30m`. |
| `output_limit_bytes` | Defaults to `524288`; accepts `4096` through `1048576`. |
| `allowed_commands` | Optional exact argv validators with explicit timeouts. Empty uses only the mandatory staged-diff check. |
| `model_provider.credential_mode` | `direct` by default or explicit `gateway`. |
| `model_provider.api` | `chat_completions` or `responses`. |
| `model_provider.endpoint`, `model` | Required complete provider coordinates. |
| `model_provider.reasoning_effort` | Optional `none`, `low`, `medium`, `high`, or `xhigh`. |
| `model_provider.auth.type` | `bearer` or `none` for direct mode; `none` for gateway mode. |
| `model_provider.public_ca_private_dns` | Fix-only acknowledgement for an explicit gateway using a privately resolved public FQDN. |

Each validator is an `argv` list plus a timeout. Shell command strings, generic
dispatchers, coding-agent re-entry, and shell interpretation are rejected. The
final validator is always the exact staged-diff check. Generation is one-shot;
validator failure cannot trigger model repair, `critique_retries` must be zero,
and `ai.fix_prs.verify` is not supported by Agent Sandbox.

Secret names, image digests, namespace, ServiceAccounts, resources, networking,
CA trust, and RuntimeClass are Helm-owned deployment settings. See:

- [Fix PR generation](fix-prs.md) for the complete maintainer workflow;
- [Agent Sandbox provider compatibility](ai-providers.md#agent-sandbox-provider-compatibility)
  for API, authentication, and endpoint constraints; and
- [Kubernetes operator reference](kubernetes-reference.md#agent-sandbox-fix-runtime)
  for the deployment contract.

## Validate a config

A one-build, discovery-only fetch validates the strict schema without making AI
calls:

```bash
./bin/aster -project-dir=../my-consumer -ai=false -builds=1
```

Then inspect the job count:

```bash
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']))"
```
