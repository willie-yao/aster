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

The pull request view reports the presubmit results already published for the
open pull requests of `branding.source_repo`. It answers "which of my open pull
requests have failing tests, and which tests are they" without opening each one
on GitHub. It is opt-in because every pass costs one GitHub listing plus
per-check bucket reads.

```yaml
pull_requests:
  enabled: true
  # Optional bounds; omit to use the engine defaults.
  max: 100            # open pull requests per pass, most recently updated first
  builds_per_job: 3   # builds listed per presubmit before the newest is selected
```

### Credentials (read this first)

Triage reads the GitHub API on every pass, so it needs a read-only token:

| Deployment | Where it runs | Credential |
|---|---|---|
| GitHub Pages | reusable workflow | `GITHUB_TOKEN`, already supplied |
| Kubernetes | worker or fetcher | `GITHUB_READ_TOKEN` |

`GITHUB_READ_TOKEN` is preferred and `GITHUB_TOKEN` is the fallback, so the
Pages path is authenticated with no action from you. On the Kubernetes path,
set `ai.githubReadToken` or `ai.githubReadTokenSecretName`; both apply whether
or not `ai.enabled` is true, because triage costs no model calls.

For a public `branding.source_repo` the token needs **no repository
privileges**. Its only job is to lift the rate limit: GitHub allows anonymous
callers 60 requests an hour against a shared per-IP budget, a personal access
token 5,000, and the Actions `GITHUB_TOKEN` 1,000 per repository. One pass
spends a paginated listing of open pull requests plus paginated changed-file
reads for each pull request with a comparable failing check. On a busy
repository a single pass exceeds 60 on its own, and the chart's default
`fetcher.watchInterval: 5m` schedules 12 passes an hour. A Pages deploy runs on
a cron, so its 1,000 is ample.

Absence is never a startup error. The fetcher logs one warning at startup, then
triage degrades to intermittent 403s while the dashboard keeps publishing, so
the only symptom is a pull request view that stops updating. `aster onboard
doctor` reports the same gap as a warning.

`discovery.include_presubmits` is not a prerequisite for pull request triage, and
turning it on does not improve attribution. Triage always resolves presubmits
from the job catalog, and attribution reads base-branch history only, so a
verdict is the same either way. The toggle only adds every presubmit to the main
job dashboard, enlarging each fetch and any enabled analysis. Enable it when you
want that dashboard coverage, not to get triage.

Draft pull requests are excluded. Each pass writes `pull-requests.json`, one
`pull-requests/<number>.json` per open pull request, and
`pull-request-failures.json` for the failures several pull requests share, and
removes detail files for pull requests that are no longer open.

A few behaviors worth knowing:

- Each failing test carries a deterministic **attribution** that compares it
  against observed results. No verdict claims a pull request caused a failure,
  because comparing observations can rule a pull request out but cannot rule one
  in. The verdicts are:

  | Verdict | Meaning |
  | --- | --- |
  | `pre_existing` | The same test is already failing on the base branch. |
  | `widespread` | The same job and test is failing on at least two other open pull requests that target the same base branch. |
  | `known_flake` | Base-branch flakiness history already classifies the test as flaky. |
  | `touches_changed_code` | Nothing explains the failure and it fails in a file the pull request changes. |
  | `unexplained` | Nothing observed rules the pull request out, so it needs investigation. |
  | `inconclusive` | No base-branch data was available to compare against. |

  Attribution runs with no model calls. It reuses the job details the same pass
  already produced plus a flakiness report recomputed over base-branch jobs only,
  so it costs nothing per failure. Presubmit history is excluded from both,
  because it describes other pull requests rather than the base branch, which is
  why publishing presubmits never changes a verdict. Cross-pull-request matching
  keys on job name, test name, **and base branch**: a build-level failure carries
  the same generic name on every job, so matching by name alone would correlate
  unrelated jobs, and one job often runs on several release branches, so matching
  without the base branch would correlate pull requests testing different code.
- A single peer is recorded as **evidence** on the verdict that does apply rather
  than issued as a `widespread` verdict of its own, except under `pre_existing`,
  which is decided before peers are consulted because a failure the base branch
  already explains does not need them. Two pull requests citing each other are
  mutually uncorroborated, and evidence that weak must not preempt base-branch
  evidence that the test passes, or take the failure out of escalation.
  `widespread` is also a point-in-time comparison against the other pull
  requests' newest builds in that pass, and its summary says so.
- `touches_changed_code` compares the source locations JUnit reported for the
  failure against the pull request's changed files. Both sides are observed, so
  the verdict states overlap and explicitly says overlap is not proof that the
  change is responsible. Every frame in the failure body is considered, because
  a stack often enters a shared framework in another repository before reaching
  the repository under test. Overlap is skipped, and its absence never claimed,
  when the failing build tested a different head than the pull request's current
  one, when the changed-file list is truncated, or when it could not be fetched.
  Failure locations inside a dependency, and version-qualified locations that
  name a tagged copy rather than the checked-out tree, are never sites.
  Source locations are recovered only for Go tests under module paths the engine
  maps to a GitHub repository, so this verdict does not apply to every project.
- A check whose build tested an older head than the pull request's current head
  is marked `stale`, so a green check on outdated code is not mistaken for a
  green check on the current one.
- A job that fails without any failing JUnit case, such as a build or verify
  step, reports one synthesized `Prow job execution` failure so every failing
  check names a subject. Those failures are never compared against the base
  branch by name, only against the same job on other pull requests.
- A pull request whose builds have aged out of the bucket's retention window
  reports `UNKNOWN` with no checks. GitHub may still show statuses for those
  runs because commit statuses outlive the artifacts.
- A triage failure never aborts the pass. The previously written view is kept
  and the dashboard still publishes.

### Shared failures

`pull-request-failures.json` publishes the cross-pull-request correlation as a
first-class object, so a failure that is nobody's fault in particular has
somewhere to be investigated. Without it, a `widespread` verdict points the
reader at a peer pull request whose page points back, and the trail ends.

- A failure is published as **shared** once **two or more** open pull requests
  report it. That is lower than the three a `widespread` verdict needs, because
  this view answers "what is hitting several pull requests" rather than "is this
  mine".
- The correlation key is the same one attribution uses: **base branch, job name,
  and test name**. The published `id` is a hash of that key, so it is stable
  across passes and survives pull requests joining and leaving the failure.
- Each member records its pull request, the build that observed the failure, and
  the verdict that member received. Members can differ: a base branch that
  already fails the test explains the failure for that pull request before peers
  are consulted.
- `oldest_build_started` and `newest_build_started` bound the member builds the
  **current pass** observed. A pass sees only the newest build per check, so
  neither is a claim about when the failure first appeared, and the view says so.
- `escalatable` reports that no member can already be analyzed from its own pull
  request. It is the cost filter: when one member offers per-pull-request
  escalation, that cheaper path is used instead of a second one here.

Clustering runs after attribution, costs no model calls, and is published on
both deploy paths. The GitHub Pages path therefore gets the aggregate view with
no server; only the analysis below needs one.

### Optional bot comment on new pull requests

Aster can post one comment on each newly opened pull request, linking to that
pull request's triage page. It reaches contributors who would never find the
dashboard on their own, which is exactly why it needs care: it is the engine's
**only unattended write that contacts a contributor's pull request**. The
scheduled pass also comments on, and when configured closes, issues it already
tracks whose finding has recovered, but that write only touches issues a
maintainer confirmed first.

It is therefore off by default, and turning it on does not post anything:

```yaml
pull_requests:
  enabled: true
  comment:
    enabled: true
    dry_run: true     # default; logs the exact body and posts nothing
    max_per_pass: 10  # default; hard cap so a bug cannot fan out
```

`dry_run` is true unless you explicitly set it to `false`. Read a real pass's
logged bodies before you change it.

#### The posting identity

Comments post as a **GitHub App**, so contributors see a bot account rather
than a person, and the credential is scoped to one repository and expires
hourly. There is no shared Aster bot: the engine runs inside your own
infrastructure, so a shared identity would mean shipping one private key that
could write to every consumer's repository.

Create the App under any account you control. It does **not** have to be owned
by the organization that owns `branding.source_repo`:

1. Create a GitHub App with **Repository permissions → Issues → Read and
   write** and **Pull requests → Read-only**, and nothing else. Pull request
   comments go through the issues API, so `issues` is what grants the write;
   pull request read access is only needed to see them on a private repository.
   Subscribe to no events; the engine polls and has no webhook receiver. Set
   **Where can this GitHub App be installed** to any account if the App and the
   repository are owned by different accounts.
2. Install it on `branding.source_repo`.
3. Generate a private key and note the App ID.

**Installing the App requires admin access to `branding.source_repo`, and that
is the real prerequisite.** Creating the App requires nothing special. A
repository admin can normally install it, though organization policy can
restrict installation to organization owners. Monitoring a repository you do not
administer, which is the common case for an upstream project, therefore needs
one of its maintainers to install the App for you.

Until it is installed the commenting pass cannot mint a token, so it logs the
failure and posts nothing. Dry run guards the step after installation: it logs
the exact bodies, so you and the repository's maintainers can read them before
any contributor sees one. User-token authentication is not supported for this
feature.

Then supply both to the fetcher:

| Deployment | Where |
|---|---|
| GitHub Pages | `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` repository secrets |
| Kubernetes | `fetcher.extraEnv` entries sourced from a Secret |

The App's bot login is its slug plus `[bot]`, for example `aster-capz[bot]`.
The fetcher logs the resolved identity at the start of every commenting pass, so
you can confirm who will post before enabling the write. Installation tokens are
minted per pass, scoped to the single repository, and expire within the hour.

#### What it will and will not comment on

A comment is posted at most once per pull request, and only for pull requests
whose triage page this pass just published, so a comment can never link to a
page the dashboard did not generate. The pass skips:

- **every pull request that existed when commenting was first enabled.** The
  first pass records the repository's highest pull request number, posts
  nothing, and only higher numbers are ever eligible. That bound is read from
  GitHub rather than from the triage listing, so a draft or a rarely-updated
  pull request outside the listing cannot be treated as new later. Numbers are
  assigned monotonically, so no clock is involved.
- pull requests the bot has already commented on
- pull requests Aster opened itself, matched by the marker in its fix pull
  request bodies rather than by author, since those are opened under a different
  credential
- draft pull requests
- pull requests that closed, merged, or became drafts since the pass began
- pull requests that have failed to post several times, so one unpostable
  thread cannot occupy the cap forever
- anything beyond `max_per_pass` in a single pass

Deduplication does not trust local state, which matters because the Pages path
keeps its data directory only in an Actions cache that expires. Three layers
each cover the one before: the activation watermark makes everything that
existed at enable time permanently ineligible, local records skip what this
deployment already posted, and every write is preceded by reading that pull
request directly, which is authoritative even when local state was lost.

Pages for pull requests that have already been commented on are kept even after
they close, so a comment cannot turn into a broken link the moment the pull
request merges. Such a page stops being refreshed and shows its own generated
timestamp. A reset data directory recovers those records from GitHub when commenting
re-activates, so the routine expiry of the Pages build cache does not drop them.
Records, and the pages they hold, are dropped after 90 days, which bounds both
the state file and the published data directory. Expiring one cannot cause a
second comment, because the pull request itself is the authoritative check.

Commenting requires that the dashboard published a triage page for **every**
open pull request. If the listing was truncated, no comment is posted and the
pass says so, because pages outside the published set are pruned and the link
would break. Raise `pull_requests.max` above the repository's open pull request
count before enabling commenting. The watermark is still recorded in that state,
so raising the cap later does not silently skip everything opened meanwhile.

Commenting is a GitHub write, so `-skip-side-effects` suppresses it along with
issue recovery and notifications. A triage failure also suppresses it, because
the pages a comment would link to were not refreshed.

One caveat on the Pages path: comments are posted during the fetch step, before
the site is built and deployed. If a later workflow step fails, a comment can
point at a page that is not live until the next successful deploy republishes
it.

There is no per-pull-request opt-out. If contributors need one, disable the
feature.

### Optional AI escalation

Every verdict above is computed without a model. When a failure stays
`unexplained`, `touches_changed_code`, or `inconclusive`, a maintainer can
escalate it for one on-demand analysis. Escalation is server-mode only and
opt-in. On the Kubernetes path, turn it on with the chart value:

```yaml
server:
  pullRequestEscalation:
    enabled: true
```

The chart renders `PULL_REQUEST_ESCALATION_ENABLED` from that value and refuses
to install when `ai.enabled` is not set. Setting the raw variable through
`server.extraEnv` is rejected. Escalation also needs the `server.actions.mode`
authentication settings, exactly like chat, and
it neither enables writes nor requires `BOT_TOKEN`. Set
`ai.githubReadTokenSecretName` so changed files are read authenticated rather
than at the anonymous rate limit. Outside the chart the server reads the
variable directly:

```bash
PULL_REQUEST_ESCALATION_ENABLED=true   # plus AI_TOKEN, AI_ENDPOINT, AI_MODEL
```

It requires `pull_requests.enabled`, an authenticated admin, and a server
started with `-project-dir`. The Pages path never offers it. The server reads
changed files with `GITHUB_READ_TOKEN`, `BOT_TOKEN`, or `GITHUB_TOKEN`, in that
order, so the `BOT_TOKEN` that admin actions already require covers it.

The contract is deliberately narrow:

- **Only the residual set is eligible.** A failure the base branch, other pull
  requests, or base-branch flakiness history already explained cannot be
  escalated from its pull request, so the free pass is the cost filter. A stale
  build is refused too, because change context would describe a different
  revision.
- **One escalation runs at a time**, no matter how many maintainers click, and
  only a few more may queue behind it. That slot is shared with shared failure
  escalation below, so the two kinds cannot run an analysis each. Admission is
  reserved before any artifact or GitHub read, so a burst of clicks cannot fan
  out into upstream requests; a start past the bound is rejected with `409`
  instead of queueing. One deadline covers the whole accepted lifetime, so a
  request that never reaches the slot fails as timed out rather than waiting
  indefinitely, and can be retried. Results are shared between admins rather
  than per-requester, so two maintainers looking at the same failure do not each
  pay for an analysis.
- **A failed escalation can be retried.** A provider error, a timeout, or a
  restart that interrupted queued work leaves the failure retryable rather than
  permanently un-analyzable, and the dashboard offers a retry in place. While a
  request key is still in the server's bounded replay index, replaying it
  returns the subject's current state instead of starting new work; the index is
  in memory, so a restart drops it.
- **The model never issues the pull request verdict.** It runs the ordinary
  agentic failure analysis under a separate module, gated by the same critique
  and judge rules, and is told explicitly not to claim the change caused the
  failure. The changed-file list is supplied only to help it locate code.
- **The analysis cache is isolated.** The module name is part of the agentic
  cache key, so an escalation never returns the dashboard's analysis of the
  same failure, or the reverse.
- **Results are private and bounded.** They are stored in
  `pr_escalation_state.json`, which is never published, retained under a cap,
  and restored after a restart. An escalation that was in flight when the
  process stopped is never restored as running: it comes back as whatever
  terminal state was last persisted for that failure, or as never started, and
  either way it can be escalated again.

### Escalating a shared failure

The same environment variable enables one escalation per **shared failure**, so
the `widespread` verdict links to an analysis instead of a peer pull request.
The two services construct independently and advertise separate capabilities, so
a server can offer one without the other.

- **The subject is the failure, not a pull request.** It is keyed by the shared
  failure's `id`, so one analysis serves every affected pull request and
  membership changes do not re-run it. The identity carries no build, so a
  finished result is checked against the build a new request would read,
  compared by repository, pull request, and build together. It is reused while
  that build is unchanged. Once the evidence moves on, the stored result stops
  being served and the dashboard offers a fresh analysis instead, so the same
  test failing again months later for a different reason is never answered with
  the old analysis.
- **Only a failure with no cheaper path is eligible.** A shared failure whose
  `escalatable` flag is false is refused, because one of its pull requests can
  analyze it directly.
- **One build supplies the evidence.** Artifacts exist per build, so the newest
  member build that finished and tested its pull request's current head is read.
  The result is attributed to the shared failure rather than to that pull
  request, and it records which build it read, because the newest usable build
  moves between passes while the shared failure keeps one identity. When no
  member has such a build, the request is refused as transient rather than as a
  verdict.
- **The model is told the failure is shared.** It runs under its own
  `sharedfailure` module: no diff is supplied, the prompt names the affected
  pull requests, and it is directed at the common cause and told not to
  attribute the failure to any pull request. The prompt also states that
  correlating on base branch, job, and test does not establish that the pull
  requests are independent, since one may be stacked on another, so the model is
  not biased away from a change that really is responsible. The module name is
  part of the agentic cache key, so the analysis never collides with the
  dashboard's or a single pull request's.
- **Everything else matches per-pull-request escalation**: admission reserved
  before any upstream read, results shared between admins, failures retryable,
  and results stored privately in `shared_failure_escalation_state.json`. The
  two kinds share one analysis slot, so the server still runs a single analysis
  at a time rather than one per kind, though each kind keeps its own queue
  bound.

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
Most projects do not need analysis tuning. The defaults are designed to work
without an `ai:` block. Add only the setting that a measured model or artifact
constraint requires:

For example, a non-Kubernetes project can remove the Kubernetes tool group:

```yaml
ai:
  tools: [filesystem]
```

The engine defaults to 15 iterations, a 5-minute per-failure timeout, two tool
calls, no byte floor, parallel tool calls, zero critique repair requests, and
the `hard` critique cache policy. Override one of those defaults only after a
measured provider or artifact constraint requires it.

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
`artifact_grounding_incomplete` warning under every policy, and a preliminary
result cannot feed patterns, corrections, remediation, actions, or Fix. Cache
policy can still change which draft is published: it gates whether a draft
reaches post-loop semantic review, and a policy-unaccepted semantic revision
cannot replace the selected draft.

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

### Experimental Agent Sandbox fix runtime

The Fix runtime is the bounded Agent Sandbox OpenCode executor described in
[Fix PR generation](fix-prs.md#agent-sandbox-opencode-executor). Agent Sandbox
remains disabled by default. Once explicitly enabled, direct provider access is
the default credential mode. The project owns generation bounds and the
non-secret provider contract:

- `max_turns`: total execution step budget, defaulting to 30;
- `timeout`: positive and at most 30 minutes, defaulting to 10 minutes;
- `output_limit_bytes`: 4096 through 1048576, defaulting to 524288;
- `allowed_commands`: optional additional post-generation validators with exact
  `argv` arrays and explicit timeouts. Aster uses only the mandatory staged-diff
  check when the list is omitted;
- `model_provider.credential_mode`: `direct` by default or explicit `gateway`;
- `model_provider.api`: `chat_completions` or `responses`;
- `model_provider.endpoint` and `model`;
- `model_provider.reasoning_effort`: optional `none`, `low`, `medium`, `high`, or `xhigh`; pinned OpenCode 1.18.2 rejects `max`;
- `model_provider.auth.type`: `bearer` or `none` for direct mode and `none` for
  gateway mode. With pinned OpenCode 1.18.2, `responses` requires direct
  bearer auth; and
- `model_provider.public_ca_private_dns`, which is valid only for an explicit
  gateway using a privately resolved public FQDN.

Removed local and cluster backend fields (`model`, `network_domains`, `agent_ref`,
`api`, `namespace`, `version`, and `retries`) are rejected. Secret name and key
are Helm deployment settings, not project settings. Direct
bearer mode requires `agentSandbox.fixRuntime.modelProvider.auth.existingSecret`
and `tokenKey`. The Secret must already exist in the execution namespace and
must hold a dedicated inference-only credential. The chart never creates,
copies, reads, or prints the Secret value.

Command strings are not accepted. Executables must be resolved through `PATH`;
shells, generic command dispatchers, and coding-agent re-entry are rejected.
Git is reserved for the exact final diff check. Arguments are passed directly
without shell interpretation, so quoting syntax has no special meaning and an
argument that contains spaces remains one `argv` element. The generic executor
supports only commands whose binaries are installed in that image.

The Agent Sandbox runtime is one-shot generation followed by validation. A
validator failure cannot trigger model repair, `critique_retries` must be 0, and
`ai.fix_prs.verify` is rejected for this runtime.

Chat Completions uses `@ai-sdk/openai-compatible`. Responses uses
`@ai-sdk/openai`, requests `store: false`, keeps complete conversation and tool
history locally, and does not use `previous_response_id`. Responses support is
endpoint- and model-dependent and deterministic tests do not establish live
provider compatibility.
Provider endpoints must use a complete HTTPS operation path matching the selected API.
Embedded credentials, queries, fragments, literal provider tokens, and local
OpenCode model fields are rejected. The Helm `agentSandbox` values must match
the project provider mode, API, endpoint, model, auth type, and trust setting
exactly while separately supplying the consumer-owned namespace, immutable
image digest, Secret reference when needed, workload ServiceAccount, resources,
and secure RuntimeClass.

AppArmor is engine-owned rather than project-configurable. Production requests
`RuntimeDefault`; no `agent_runtime` field can disable it or select
`Unconfined`.
