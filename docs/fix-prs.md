# Agent-proposed fix PRs

The dashboard can draft a **minimal code fix** for a systemic recurring failure
and open a **draft pull request** against the source repo. It is **off by
default**, opt-in per project, and heavily guardrailed: draft-only, bounded file
scope, a CLA-signed commit author, and idempotent dedup.

This is the highest-risk automation the engine offers (it writes code to a repo),
so read this whole page before enabling it.

The standard deployments do not include the coding-agent runtime. Scheduled and
interactive fix generation require `opencode` and git in the process that runs
the feature. File issue and Mark resolved do not have this requirement.

## What it does

After each fetch, for every **systemic** recurring pattern (the same ones
surfaced on the home page) at or above `min_confidence` that carries a concrete
suggested fix, the engine:

1. Runs a **coding agent** (`opencode`) in a real clone of the source repo at a
   pinned commit. The agent investigates the tree and makes the **minimal
   change** that addresses the root cause; it can touch several files coherently
   and, when `allow_bash` is set, build and test its own change while fixing.
2. **Reviews** the change with a second LLM call (a skeptical reviewer that flags
   wrong logic, undefined symbols, or a change that doesn't address the root
   cause). On objections it re-runs the agent with the feedback up to
   `critique_retries` times, then drops the fix.
3. Optionally **verifies** the change (build + vet) when `verify` is set,
   stamping the verdict on the PR without ever blocking the draft.
4. Opens a **draft PR** via fork-and-PR with the change, the diff, and a review
   checklist in the body.

A fix that can't be produced (the agent makes no change, touches more than
`max_files`, or the reviewer keeps rejecting it) is dropped and logged. No
partial or speculative changes are ever pushed.

The authenticated action controls run a deterministic eligibility check before
draft generation. Investigation-only targets direct the maintainer to source
investigation, missing or malformed targets show that more evidence is required,
and pinned-source verification reports when the remediation already exists. A
blocked state does not create an action request, call a model, start an Orka
Task, or send a draft-ready notification. Draft generation repeats verification
and remains authoritative.

> **Note on correctness.** The engine bounds the change (minimal scope, at most
> `max_files`), runs an LLM review, and optionally builds it, but it does not
> guarantee the change fixes the failure. A fix PR is a reviewed **draft starting
> point**, not a verified patch; Prow CI and a human reviewer are the correctness
> gate (a draft PR won't run CI or merge without a maintainer's approval).

## Two modes: fork-and-PR vs direct

How the fix branch reaches the source repo depends on whether you can write to
it, controlled by `ai.fix_prs.fork` (default `true`):

- **`fork: true` (default): fork-and-PR.** For a source repo you **don't** own
  (the usual case: an upstream community repo). The engine forks the repo under
  the token's identity, pushes the branch to that fork, and opens a **cross-fork
  PR** against the source repo.
- **`fork: false`: direct.** For a source repo you **do** own or maintain (e.g.
  a team running the dashboard on its own CI). The engine pushes the branch
  straight to the source repo and opens a **same-repo PR**. No fork involved.

Either way the PR targets the source repo's default branch and is opened as a
draft. The branch is **never** pushed into a repo you don't own.

## Identity, CLA, and the token (read this first)

- **`FIX_TOKEN`** is a **personal access token** of a real contributor. It is
  **not** the Actions `GITHUB_TOKEN` (which can't touch a fork elsewhere). Which
  PAT kind you need depends on the mode:
  - **`fork: true` against a repo you don't own** → use a **classic PAT** (scope
    `repo`, or `public_repo` for public-only repos). A **fine-grained PAT cannot
    open a PR against a repo you don't own**, because it can only be granted
    permissions on your own repos.
  - **`fork: false` against a repo you own** (or `fork: true` testing against
    your own fork) → a **fine-grained PAT** works: scope it to that repo with
    **Contents: Read and write** and **Pull requests: Read and write**.
- **CLA / DCO.** CNCF projects (Kubernetes, etc.) run EasyCLA, which checks
  **every commit's author** against a signed CLA and blocks merge otherwise. So:
  - `author_name` / `author_email` **must** be the CLA-signed identity, and the
    email **must** match that GitHub account, or the check reports an "unknown
    commit author".
  - Every commit gets a DCO `Signed-off-by` trailer matching the author
    (required by Kubernetes repos). The engine adds this automatically.
  - A GitHub App / bot identity generally is **not** recognized by EasyCLA;
    use a human contributor's PAT.
- **Prow keeps a human in the loop for free.** A draft PR won't run CI or merge
  without a maintainer's `/ok-to-test`, `/lgtm`, and `/approve`. The engine never
  merges anything.

## Configuration

```yaml
ai:
  fix_prs:
    enabled: true
    # repo:                       # defaults to branding.source_repo
    #   owner: "kubernetes-sigs"
    #   name: "cluster-api-provider-azure"
    author_name: "Jane Maintainer"     # required: CLA-signed identity
    author_email: "jane@example.com"   # required: must match that GitHub account
    # fork: true                  # true (default): fork-and-PR for a repo you don't own;
    #                             # false: direct branch + same-repo PR for a repo you own
    # min_confidence: high        # only systemic patterns at >= this confidence (default high)
    # max_files: 3                # cap files a single fix may touch (default 3)
    # max_new_per_run: 1          # cap fix PRs per fetch (default 1)
    # labels: [ai-proposed-fix]   # labels applied to each PR
    # dry_run: false              # propose without opening a PR (see below)
    # critique_retries: 1         # LLM review re-prompts before dropping (default 1)
    # verify:                     # build/vet the change before opening the PR (see below)
    #   enabled: false            # off by default; needs a git + language toolchain on the runner
    #   commands:                 # override the default; each line is one command (no shell)
    #     - go build ./...
    #     - go vet ./...
    #   timeout: 10m              # per-command bound
```

`enabled: true` requires `author_name` and `author_email` (validated at load).
The feature is active only when **all** of `enabled: true`, a non-empty
`FIX_TOKEN`, and a resolved source repo are present; any missing piece is a
no-op, never a deploy failure.

### The LLM review (`critique_retries`)

After the agent produces a change, a second LLM call reviews it as a skeptical
reviewer and returns concrete defects (not style). If it objects, the engine
re-runs the agent with that feedback, up to `critique_retries` times (default 1),
then drops the fix. The review uses the same AI client as generation.

### Coding-agent generator (`agent_runtime`)

The fix generator is selected by `agent_runtime.type`. The reviewer, verification,
dry-run preview, scope limits, and PR-opening path remain engine-owned for both
backends.

#### `opencode` (default)

The local backend runs the `opencode` CLI in a real clone on the runner. It uses
the engine's `ai.endpoint`, `ai.model`, and `AI_TOKEN`.

```yaml
ai:
  fix_prs:
    enabled: true
    author_name: "Jane Maintainer"
    author_email: "jane@example.com"
    agent_runtime:
      type: opencode
      model: ""
      max_turns: 30
      allow_bash: true
      timeout: 10m
```

The runner needs `opencode` and git. The standard distroless Kubernetes image
does not contain them. Local `opencode` deployments should build
[`deploy/fixer.Dockerfile`](../deploy/fixer.Dockerfile), which installs both.
The chart's `orka.fixRuntime.enabled` image contains git only because it
reconstructs diffs returned by an Orka Agent; it does not support the local
`opencode` backend.

#### `orka` (in-cluster)

See [Orka architecture in prow-ai-dashboard](orka-architecture.md) for how the
Agent Task, isolated workspace, structured result, and dashboard validation
boundary fit with the other Orka integrations.

The dashboard runtime type `orka` selects the execution backend. The referenced
Orka `Agent` selects its coding CLI independently. For example,
`spec.runtime.type: opencode` selects OpenCode without adding a dashboard runtime
type such as `orka-opencode`.

The backend creates a generation-only `type: agent` Task in an isolated
workspace. Orka captures the final workspace and returns its structured result
with `version`, `summary`, the pinned `baseSHA`, a unified `diff`, and the changed
`files`. The model's final message remains the human-readable summary. The Task
does not receive `FIX_TOKEN`, push a branch, or open a PR. The engine strictly
decodes that result, verifies the reported base SHA and file list, reapplies the
diff to the pinned base, rejects deletions, binary files, symlinks, submodules,
and `.gitmodules` edits, then runs the normal review, verification, preview, and
PR-opening stages.

```yaml
ai:
  fix_prs:
    enabled: true
    author_name: "Jane Maintainer"
    author_email: "jane@example.com"
    agent_runtime:
      type: orka
      agent_ref: opencode-fixer
      api: http://orka.orka-system.svc:8080
      namespace: orka-system
      version: v1
      retries: 1  # default; set 0 to disable Orka Task retries
      max_turns: 30
      allow_bash: true
      timeout: 15m
```

The Orka `Agent` and its model Secret are operator-owned. Start from
[`configs/example/orka-opencode-agent.yaml`](../configs/example/orka-opencode-agent.yaml).
The Secret must contain `OPENAI_BASE_URL`; add `OPENAI_API_KEY` only when the
endpoint requires authentication. Set `spec.model.name` to the endpoint-specific
model ID and `spec.runtime.type` to `opencode`. Do not copy the endpoint, model,
or model credential into `project.yaml`.

Guarded fix Tasks do not accept `git_secret` or any other Task-level credential
reference. The source repository must currently be publicly cloneable. Private
repository support requires an upstream Orka credential binding that admission
can pin without allowing an arbitrary Secret name. Keep the write-capable
contributor token in `FIX_TOKEN`; it remains inside the dashboard and is used
only by the engine-owned PR-opening path.

For Helm deployments, set `orka.fixRuntime.enabled=true` and repeat the exact
Agent, repository, turn, Bash, timeout, and retry settings in the admission
contract:

```yaml
orka:
  namespace: orka-system
  fixRuntime:
    enabled: true
    admission:
      agentRef: opencode-fixer
      repository:
        owner: example
        name: repo
      maxTurns: 30
      allowBash: true
      timeout: 15m
      retries: 1
```

The chart uses the published git-capable fixer image, mounts a ServiceAccount
token only into the workloads that generate fixes, and grants a separate
Task-only fix Role. It also installs a fail-closed requester-scoped admission
policy. A mismatch between `project.yaml` and Helm values denies the Task. REST
authentication uses `ORKA_API_TOKEN`, `ORKA_API_TOKEN_FILE`, or the pod
ServiceAccount token. File-backed credentials are read for every result request,
so projected ServiceAccount token rotation does not require a server restart.
Local kubeconfig testing can select a context through `ORKA_KUBE_CONTEXT`.

The policy matches the authenticated dashboard ServiceAccount in
`orka.namespace`; labels alone cannot opt a request out. It pins the Agent and
namespace, public GitHub repository, immutable 40-character commit, workspace
shape, turns, Bash setting, timeout, retries, priority, Agent-owned resource
bounds, and dashboard identity metadata. It rejects custom images, commands,
arguments, environment variables, Secret references, mutable refs, workspace
push settings, scheduling, sessions, webhooks, prior Tasks, tool overrides, and
placement overrides. The referenced Agent is operator-owned, so protect changes
to its runtime, Secret, tools, and resource settings separately.

Source investigation and fix actions cannot currently share one server pod. A
single pod has one Kubernetes requester identity, so admission cannot distinguish
the two Task contracts without trusting caller-controlled labels. Deploy them
separately until the runtimes have distinct requesters. Container analysis is
unaffected because it uses a dedicated Task namespace and keeps its existing
admission policy.

OpenCode support requires upstream PR #289. The generated chart, all 12 CRDs,
complete AgentRuntime and SubstrateActorPool controller RBAC, and guarded CRD
lifecycle are present at Orka merge commit
`fde3b7925c367784570fcc36d7a5b3a51747bf10` from PR #295.

This repository does not configure a verified published Orka release containing
those changes. Do not install older raw manifests or add a supplemental
controller RBAC patch. Source-commit packaging is maintainer-only and limited to
local lint, render, and temporary kind validation until matching immutable
runtime artifacts are available. See [Orka integration](orka.md) and the
[CAPZ Orka consumer reference](https://github.com/willie-yao/capz-prow-ai-dashboard-orka-demo/tree/main/deploy/orka).

Orka labels the project experimental, so treat this integration accordingly.
The dashboard ServiceAccount must remain limited to Orka Tasks; controller and
worker RBAC stays with the separate Orka release.

Fix generation remains independent from failure analysis. Failure analysis runs
in-process by default; Helm deployments may separately select the experimental
`orka-container` analysis runtime without configuring or changing fix generation.

The Orka runtime can generate without the engine chat-completions client only
when `critique_retries: 0` is explicitly configured. A positive retry count fails
closed unless the normal AI reviewer is available. Template formatting also
requires that client. Enable `verify` so the returned change is tested again in a clean engine-owned
workspace rather than trusting the agent's own test run.

### Verification (`verify`)

When `verify.enabled` is set, the engine builds and vets the proposed change
before opening the PR: it checks out the source repo at the pinned base, overlays
the edited files, and runs the verify commands (default `go build ./...` then
`go vet ./...`). The verdict is stamped on the PR body and the confirm preview:
"passed", "failed" (the diff likely does not build; the draft is still produced
as a lead), or "skipped". This catches the common failure modes of a generated
diff, a hallucinated API or a broken signature, that syntax parsing alone misses.
It does not reproduce the CI failure itself; that is left to the PR's own
presubmits.

Verification is annotate-only: a build failure never blocks the draft. It needs
git and the language toolchain on the runner, so it runs in a full CI runner or
a dev host; on the distroless server/worker image it reports "skipped". Execution
runs the edited repo's build, so enable it only for a source repo you trust. The
build resolves the repo's dependencies, so the runner needs network access (or a
warmed module cache); a fetch failure surfaces as a "failed" verdict, so set a
`timeout` that accommodates the first cold build.


## Closed-loop Prow verification

After the dashboard opens a fix pull request, it keeps a private remediation
ledger and follows the pull request through Prow and GitHub. Consumers do not
configure test names, trigger commands, pull numbers, or periodic-to-presubmit
mappings.

The engine reads this metadata automatically:

- Prow job definitions from the pinned `kubernetes/test-infra` configuration.
- Pull request refs, head SHA, base SHA, rerun command, status, and build URL
  from `prowjob.json`.
- Checkout metadata from `started.json` and `finished.json`.
- Test names, results, and failure signatures from JUnit XML.
- Pull request merge state and commit ancestry from GitHub.

For a presubmit finding, only a run of the same job on the current pull request
head counts. A new commit invalidates older results. For a periodic finding, the
engine builds a private coverage index from recent presubmit JUnit reports. An
exact matching test can provide pre-merge evidence without a hand-written job
mapping. The original periodic remains authoritative after merge.

A periodic build counts only when its tested source commit contains the merged
change. A later timestamp alone is not sufficient. Persistent findings require
two clean post-merge runs by default. Flaky findings require ten clean
opportunities. Missing JUnit, missing source SHAs, or repository mismatches stay
in an inconclusive state.

The pull request must target a repository tested by the Prow job. For an
upstream community project, configure the upstream repository with `fork: true`.
Pointing `repo` at a personal fork creates a pull request that upstream Prow does
not test.

When the same failure signature recurs after merge, the remediation returns to a
failed state and the linked issue remains open. Follow-up fix generation is a
separate feature and is not part of this verification lifecycle.

`remediation_state.json` and `remediation_prow_catalog.json` are private
operational files. `remediations.json` is a redacted public projection used by
the dashboard to show pull request and verification status.

Wire the token into the deploy workflow:

```yaml
# .github/workflows/deploy.yml  (GitHub Actions + Pages path)
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      runs-on: fix-enabled-runner   # preinstalled opencode + git
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      FIX_TOKEN: ${{ secrets.FIX_TOKEN }}
```

On the [Kubernetes-native](kubernetes-reference.md) path, set `FIX_TOKEN` on the worker via
`fetcher.extraEnv` in the Helm values instead. The writer still needs a custom
runtime image as described above. An admin can draft a single fix PR on demand
only when the server image also contains `opencode` and git (see
[server.md](server.md#admin-gated-actions)).

## Start with dry-run

Before letting it open real PRs, set `dry_run: true`. The engine runs the full
pipeline (locate, fetch, edit, validate) and writes the proposed changes to
`fix_previews.json` in the fetcher's output directory and logs the diffs, but
**opens no PR and forks nothing**. Inspect the previews, confirm the edits look
right and target the correct files, then flip `dry_run` off.

`fix_previews.json` is operational state. Pages removes it before publication
and the Kubernetes server returns 404 for it. Inspect it in a local output
directory, the persistent volume, or the fetch logs rather than through the
dashboard URL.

## Following the repo's PR template

When the source repo has a pull-request template (`.github/PULL_REQUEST_TEMPLATE.md`,
`PULL_REQUEST_TEMPLATE.md`, or `docs/PULL_REQUEST_TEMPLATE.md`), the engine
reformats the generated PR description to follow it with one extra AI call: it
fills the template's sections from the proposed change, keeps placeholder text
and checklists you have no information for, and picks a single best-fit Prow
`/kind` line when the template has one. The warning banner, rendered diff,
dashboard link, and dedup marker are always preserved. No template (or no AI
configured) falls back to the default body, and any error during reformatting
silently uses the default. This is automatic; there is no flag to set. Fetching
the template uses `FIX_TOKEN`, which already has Contents read on the source repo.

## Guardrails (summary)

- **Opt-in** per project; **draft-only** PRs; never pushes to a protected branch.
- Only **systemic**, at-or-above-`min_confidence` patterns with a concrete fix.
- A **coding agent** makes the change in a real clone; bounded by `max_files` and
  gated by the LLM review.
- Dedicated **`FIX_TOKEN`** with a CLA-signed author and DCO sign-off.
- **Idempotent**: a hidden marker keyed by job + root-cause fingerprint (local
  state plus an open-PR search) means a pattern is never proposed twice, and a
  different cause on the same job is proposed separately.
- **`max_new_per_run`** caps PRs per fetch.

## Known limitations

- **File mode.** Edited files are committed as regular files (`100644`). If a fix
  were to edit an executable script, the PR would drop the executable bit; the
  change is visible in the draft diff for a reviewer to catch. Fix targets are
  typically YAML/templates, so this is rare.
- **Concurrency.** Dedup (local state + an open-PR search) is not atomic, so two
  overlapping deploys could both propose the same fix. Scheduled deploys are
  normally serialized; add a workflow `concurrency:` group if you run them in
  parallel.
- **First fork.** Creating a brand-new fork is asynchronous; on the very first
  run for a never-forked repo the commit step may fail while the fork populates.
  The next run (fork now exists) succeeds.

## Relationship to the other features

This builds on the same pattern analysis that drives the home-page recurring
patterns and the auto-filed issues ([github-issues.md](github-issues.md)).
Issues act on **your** repos; fix PRs are the only feature that writes to the
**source** repo, which is why the identity and CLA requirements are stricter.

## Retained patterns

Fix generation requires a fresh `current` pattern with current evidence. A retained last-known-good pattern remains visible with its existing remediation references, but it cannot start a new issue, fix preview, or remediation attempt.

## Individual build failures

A completed failed run that has an accepted `source: "build"` analysis can use the same authenticated preview and confirmation flow without being converted into a recurring pattern.

- **File issue** renders single-run language with the Prow build, build log, published root cause, and suggested remediation.
- **Propose fix** requires at least one repository path that the analysis linked to the configured source repository. The coding agent inspects the pinned repository and must produce a repository change. If the evidence supports only an external platform or operator action, or the agent produces no code change, the preview is rejected while issue preview remains available.
- Build previews use a content hash over the job, build, typed subject identity, analysis generation, root cause, suggested fix, and relevant files. Confirmation fails closed when any of that published analysis changes or leaves the current window.
- Build issues and fixes use GitHub markers for deduplication and are removed from the recurring-pattern tracking files after confirmation. They do not create a one-build pattern or participate in recurring-pattern remediation state.

The server advertises its current analysis critique version with the action capability. The frontend hides build action controls when the published analysis predates that contract, while analyses produced by a newer compatible engine remain visible during a rollback or rolling upgrade.
