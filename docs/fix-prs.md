# Experimental Fix PR generation

> **Status: experimental.** Fix PR generation uses the Agent Sandbox runtime
> only. It is not part of standard onboarding. Analysis chat, File Issue, and
> Mark Resolved are separate server features and do not require a Fix PR runtime.

The dashboard can draft a **minimal code fix** for a legacy action-capable
recurring failure or one exact failed JUnit analysis with a selected
evidence-backed chat response, then open a **draft pull request** against the
source repo. It is **off by
default**, opt-in per project, and heavily guardrailed: draft-only, bounded file
scope, a CLA-signed commit author, and idempotent dedup.

This is the highest-risk automation the engine offers (it writes code to a repo),
so read this whole page before enabling it.

Fix PR generation requires a consumer-installed Agent Sandbox executor. Agent
Sandbox runs OpenCode and every target-controlled validation command only in the
isolated executor workload. File Issue and Mark Resolved do not require a Fix PR
runtime.

## Analysis-only causal groups

Causal-group correlation remains analysis-only. It does not emit a suggested fix,
remediation target, source target, or action field. The public pattern instead
carries separate remediation-investigation summaries keyed to engine-derived
causal-group IDs and content hashes. Repeated groups start at `not_investigated`
with the message that no source-grounded implementation target has been verified.

The job detail view shows this state in a concise, always-visible
**Remediation** section. Authenticated server deployments may explicitly enable
**Investigate possible fix**, which starts the bounded trusted read-only
investigator and publishes only safe status. Technical details remain collapsed.
Existing File Issue and Fix PR gates continue to reject causal-group results, and
remediation state is excluded from causal-group and pattern content hashes so an
investigation cannot rewrite the published analysis identity.

## Exact JUnit analysis handoff

Authenticated server deployments using the Agent Sandbox Fix runtime offer
**Start fix investigation** for an exact failed JUnit analysis. This creates a
fresh owner-bound session instead of restoring the latest normal chat, and its
turns send `fix_intent: true` so the server performs immutable source and path
preflight before contacting the provider. The control does not generate a
preview, branch, or pull request. Normal **Chat with agent** continues restoring
the latest existing conversation.

After a successful cited response, **Use this finding in a fix proposal** remains
the separate explicit action that admits a persisted asynchronous preview
request. The server returns the owner-bound request before Agent Sandbox
generation finishes, and the UI polls the same request until the preview is
ready. Closing the dialog, losing the browser connection, or exceeding an edge
proxy's HTTP timeout does not cancel an already admitted Sandbox. Reopening the
dialog restores the request from same-origin session storage; repeating the same
admission input reconnects to the active request instead of starting another
Sandbox. This path does not use a
recurring pattern as action authority. It requires:

- one failed JUnit case with a current accepted published analysis;
- one owner-bound successful chat turn with validated artifact citations;
- an exact repository and full commit resolved from build metadata;
- published verified source links for that same repository and revision;
- a selected finding that names an explicit backticked source symbol;
- deterministic verification of that symbol as present or absent in the
  bounded pinned source, plus a source snapshot hash; and
- the configured Fix PR destination to match the analyzed repository.

The exact JUnit path uses the dashboard's immutable GitHub source reader and
deterministic source verification. Legacy pattern chat-to-fix keeps its existing
verified source and target requirements.

The chat session persists a full authoritative analysis content hash that covers
failure content, artifact citations, verified source links, critique state, and
analysis provenance, together with the exact source repository and revision.
Any later change requires a new chat session. During an active turn the browser
retains only the analysis identity, session ID, request ID, and Fix-intent bit in
same-origin session storage. Reload reconnects with the same request identity. If
the intent bit is unavailable, the client polls the admitted request instead of
resubmitting it without Fix intent.

Fix generation starts only when the pinned build revision is still the target
repository's default-branch head. Confirmation rechecks the owner-bound chat
response, full analysis hash, source snapshot, symbol-grounding result,
destination configuration, branch head, canonical patch reconstruction, and the
retained exact executor command results
before any GitHub write. The Agent Sandbox request receives no GitHub token. The
dashboard does not rerun target repository build, test, vet, or validation
commands during preview or confirmation.

The persisted asynchronous request does not replace the existing preview and
confirmation stores. It only owns admission, background execution, polling, and
runtime cleanup. The completed preview still passes through the existing source,
patch, reconstruction, command-result, owner, authentication, confirmation, and
GitHub deduplication gates.

## Command execution and credential boundary

The following boundary is enforced without a provider call. It follows the
server, worker, fetcher, runtime, executor, Docker, and Helm contracts directly.

| Process | Agent Sandbox Fix behavior | Environment, storage, network, and credentials |
|---|---|---|
| Dashboard server | Dispatches the Sandbox, reconstructs the canonical patch, validates files, identities, targets, and retained command results, then performs a separately confirmed GitHub write. It does not run target build, test, vet, or validation commands. | The server can hold `BOT_TOKEN`, AI and OAuth credentials, mounts the shared `/data` PVC, project configuration, and `/tmp`, and has normal dashboard egress. Target code never runs in this process. |
| Worker or fetcher | Dispatches scheduled Sandbox generation, validates returned results, reconstructs the patch, and may open the configured draft PR. It does not run target commands for Agent Sandbox. | The process can hold `FIX_TOKEN`, AI credentials, shared `/data`, project configuration, and its normal network access. None of these enter the Sandbox request. |
| `remote-fixer` image | Supplies the normal dashboard binaries and git for patch reconstruction. The included Go toolchain is not used to execute target code. | It inherits the server, worker, or fetcher Pod boundary. It is not a separate execution workload and receives no target command. |
| Agent Sandbox executor | Clones the public pinned source, runs OpenCode once, stages the patch, then runs every configured exact validator and the final `git diff --cached --check`. | The workload mounts only bounded `/workspace` and `/tmp` `emptyDir` volumes. ServiceAccount token automount is disabled and no GitHub credential or dashboard PVC is present. Validation children receive only HOME, temp, PATH, locale, and CA variables. OpenCode state is removed before validation, the provider token is not inherited, and the parent is non-dumpable so validators cannot read the request or credential through `/proc`. Egress is governed by the consumer-owned execution namespace and network policy. |
| Local command verifier | Clones, overlays, and executes trusted verification commands directly with `os/exec` where that internal path is explicitly used. | No isolation beyond the local workspace. It is separate from Agent Sandbox generation and must not run untrusted target code. |

Agent Sandbox confirmation repeats identity, source, symbol, target, destination,
base, command-result, and canonical patch checks. It never repeats target command
execution in the dashboard process.

## What it does

After each fetch, for every **systemic** recurring pattern (the same ones
surfaced on the home page) at or above `min_confidence` that carries a concrete
suggested fix, the engine:

1. Runs a **coding agent** inside Agent Sandbox in a real clone of the source
   repo at a pinned commit. The agent investigates the tree and makes the
   **minimal change** that addresses the root cause. Bash remains disabled.
2. Requires `critique_retries: 0`: Agent Sandbox performs one generation request,
   then exact post-generation validation with no repair retry.
3. Requires the complete ordered successful result for every
   `allowed_commands` entry, including the final staged diff check.
4. Opens a **draft PR** via fork-and-PR with the change, the diff, and a review
   checklist in the body.

A fix that can't be produced (the agent makes no change, touches more than
`max_files`, or validation fails) is dropped and logged. No partial or
speculative changes are ever pushed.

Action availability uses a stable machine-readable reason code in addition to an operator-facing explanation. Retained or inactive patterns, non-systemic results, incomplete contracts, unsafe remediation, and inconclusive source verification remain distinct. The same code is used by eligibility checks, synchronous action errors, asynchronous requests, restored drafts, and confirmation-time revalidation. Older payloads without a code remain supported.

The authenticated action controls run a deterministic eligibility check before
draft generation. Investigation-only targets show that more source grounding is
required, missing or malformed targets show that more evidence is required, and
pinned-source verification reports when the remediation already exists.
Behavior targets are actionable only when the target function and the proposed
package-level callee both exist in bounded pinned source. An external dependency
or an incomplete or ambiguous package remains investigation-only. The shared
remediation policy additionally rejects destructive CRD conversion changes and
false claims that admission webhook cleanup disables conversion. The same policy
runs before generation and when a persisted preview is restored or confirmed. A
blocked state does not create an action request, call a model, start a Sandbox,
or send a draft-ready notification. Draft generation repeats verification and
remains authoritative.

> **Note on correctness.** The engine bounds the change (minimal scope, at most
> `max_files`). Agent Sandbox runs exact post-generation validators with no
> repair retry. Configured validators may build the patch, but none of these
> guarantees the change fixes the failure. A fix PR is a **draft starting point**,
> not a verified patch; Prow CI and a human reviewer are the correctness gate (a
> draft PR won't run CI or merge without a maintainer's approval).

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
    #   owner: "example-org"
    #   name: "source-project"
    # allowed_repositories:       # explicit cross-repository destinations
    #   - owner: example-org
    #     name: ci-config
    #     path_prefixes:
    #       - config/jobs/example-org/source-project/
    #     allowed_commands:
    #       - argv: [git, diff, --cached, --check]
    #         timeout: 1m
    #     fork: true
    author_name: "Jane Maintainer"     # required: CLA-signed identity
    author_email: "jane@example.com"   # required: must match that GitHub account
    # fork: true                  # true (default): fork-and-PR for a repo you don't own;
    #                             # false: direct branch + same-repo PR for a repo you own
    # min_confidence: high        # only systemic patterns at >= this confidence (default high)
    # max_files: 3                # cap files a single fix may touch (default 3)
    # max_new_per_run: 1          # cap fix PRs per fetch (default 1)
    # labels: [ai-proposed-fix]   # labels applied to each PR
    # dry_run: false              # propose without opening a PR (see below)
    critique_retries: 0         # required for Agent Sandbox
    agent_runtime:
      type: agent-sandbox       # default and only supported runtime
      allow_bash: false         # required
      max_turns: 30
      timeout: 10m
      allowed_commands:
        - argv: [git, diff, --cached, --check]
          timeout: 1m
```

`enabled: true` requires `author_name` and `author_email` (validated at load).
The feature is active only when **all** of `enabled: true`, a non-empty
`FIX_TOKEN`, and a resolved source repo are present; any missing piece is a
no-op, never a deploy failure.

Repository-qualified remediation targets are rejected unless the destination
is listed in `allowed_repositories` and every changed path is under one of its
prefixes. Prow environment changes additionally pin the exact
`kubernetes/test-infra` discovery revision, job, container, variable name, and
replacement value. The engine parses the YAML and fails closed on duplicate
jobs, duplicate variables, `valueFrom`, or ambiguous containers. Prow assigns
the effective name `test` to a job with one container, including when the source
YAML omits the container name, so remediation targets use `container: test` for
that common form.
Agent Sandbox destinations also require their own exact `allowed_commands`;
validators from the default repository are never reused implicitly for a
different repository.

### Generation retries (`critique_retries`)

Agent Sandbox requires `critique_retries: 0`. The engine performs one generation
request and then exact post-generation validation. Positive values fail
configuration validation.

### Coding-agent generator (`agent_runtime`)

`agent_runtime.type` accepts only `agent-sandbox`, which is also the default.
`allow_bash` defaults to `false` and must remain `false`. Removed local and
cluster backend fields (`model`, `network_domains`, `agent_ref`, `api`,
`namespace`, `version`, and `retries`) are rejected. Scope limits, independent
patch reconstruction, result validation, preview, and PR opening remain
engine-owned. Agent Sandbox validators run in the executor, not in the dashboard
process.

The Agent Sandbox executor returns a patch to the dashboard. Kubernetes
deployments use the minimal `remote-fixer` engine image to reapply that patch to
the pinned source revision and reconstruct the exact changed-file map. This
image contains git but does not contain OpenCode or model credentials.

### Agent Sandbox validation

`ai.fix_prs.verify` is rejected for Agent Sandbox. Put exact post-generation
validators in `agent_runtime.allowed_commands`; the final command must be
`git diff --cached --check`. The executor runs validators before returning its
result. Dashboard processes reconstruct and validate the retained command results
but do not execute target build, test, vet, or validation commands during preview
or confirmation.

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

On the [Kubernetes-native](kubernetes-reference.md) path, set `FIX_TOKEN` on the
worker via `fetcher.extraEnv` and on the server through `server.extraEnv` when
on-demand previews are enabled. Also enable `agentSandbox.fixRuntime` with the
matching executor image, provider, and command contract described below. Static
Pages deployments do not run Agent Sandbox Fix generation.

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
- A **coding agent** makes the change in a real clone and is bounded by
  `max_files`. Agent Sandbox is one-shot and uses exact post-generation
  validators.
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

### `agent-sandbox` OpenCode executor

The experimental `agent-sandbox` runtime creates one cold Kubernetes SIG Agent
Sandbox `v1beta1` resource per Fix PR request. Agent Sandbox remains disabled by
default. The consumer installs and upgrades the controller separately. The
dashboard chart never installs the controller, CRD, a secure RuntimeClass, node
infrastructure, or provider egress infrastructure.
The [secure-runtime contract](kubernetes-platform.md#secure-runtime-contract) is
provider-agnostic and applies only when an Agent Sandbox feature is enabled.

After the runtime is explicitly enabled, `direct` is the default credential
mode. Direct bearer mode gives the OpenCode process access to one dedicated
inference credential. The Secret must already exist in the execution namespace
and is referenced through exactly one `secretKeyRef`. Direct unauthenticated
mode uses `auth.type: none` and renders no Secret reference. Explicit `gateway`
mode retains the tokenless consumer-operated gateway behavior.

A project configuration is explicit and fail closed:

```yaml
ai:
  fix_prs:
    enabled: true
    author_name: "Jane Maintainer"
    author_email: "jane@example.com"
    max_files: 3
    critique_retries: 0
    agent_runtime:
      type: agent-sandbox
      max_turns: 30
      allow_bash: false
      timeout: 10m
      output_limit_bytes: 524288
      allowed_commands:
        - argv: [git, diff, --cached, --check]
          timeout: 1m
      model_provider:
        credential_mode: direct
        api: chat_completions
        endpoint: https://api.githubcopilot.com/chat/completions
        model: claude-sonnet-4.6
        reasoning_effort: high
        auth:
          type: bearer
```

The matching Helm values add only the existing Secret reference:

```yaml
agentSandbox:
  fixRuntime:
    enabled: true
    modelProvider:
      credentialMode: direct
      api: chat_completions
      endpoint: https://api.githubcopilot.com/chat/completions
      model: claude-sonnet-4.6
      reasoningEffort: high
      auth:
        type: bearer
        existingSecret: agent-sandbox-model
        tokenKey: AI_TOKEN
      publicCAPrivateDNS: false
```

For native Responses, change both project and Helm `api` values to `responses`
and use a full endpoint such as `https://api.openai.com/v1/responses`. Pinned
OpenCode 1.18.2 maps Chat Completions to `@ai-sdk/openai-compatible` and
Responses to `@ai-sdk/openai`. The optional project `reasoning_effort` and Helm
`reasoningEffort` values must match. Pinned OpenCode 1.18.2 emits the expected
wire field for `none` through `xhigh`; it rejects `max`. Responses currently
requires direct bearer auth; Chat Completions retains direct unauthenticated and
explicit tokenless gateway modes.

Use a dedicated inference-only credential. Do not mount `BOT_TOKEN`,
`FIX_TOKEN`, OAuth credentials, GitHub read credentials, or a general GitHub
PAT. The dashboard never reads the Secret value. Helm never creates, copies, or
prints it. Admission fixes the Secret name, key, environment variable, auth
mode, executor image, and complete environment shape. It continues to reject
`envFrom`, Secret volumes, projected tokens, extra credentials, and arbitrary
environment entries.

OpenCode configuration contains only
`{env:PROW_AI_MODEL_PROVIDER_TOKEN}` for bearer mode, never the token. The
versioned execution request contains the provider mode, API, endpoint, model,
auth type, and fixed environment name but no Secret name, key, value, or value
hash. The executor rejects any exact credential found in output, summaries,
patches, changed-file content, command output, structured results, or failure
data before publication.

Gateway mode requires `auth.type: none`. Private-CA provider gateways may use
the optional public ConfigMap bundle documented in
[Kubernetes reference](kubernetes-reference.md#agent-sandbox-fix-runtime). The
Fix runtime can explicitly acknowledge a privately resolved public gateway FQDN
with `public_ca_private_dns: true`; direct provider endpoints use direct mode
instead.

Responses requests use `store: false`, keep the complete conversation and tool
history in the local OpenCode session, and omit `previous_response_id`.
Deterministic OpenCode 1.18.2 tests prove Responses streaming text, native tool
calls, StructuredOutput in the analyzer, and the Fix edit path. They do not
establish live compatibility with every Responses-like provider or model.

Generation is one-shot. OpenCode Bash, web fetch, task delegation, external
skills, and external-directory access are disabled. After OpenCode finishes,
the executor stages the patch and runs the configured exact argv validators with
a credential-free environment. A validator failure
returns a terminal failed result. The dashboard requires the complete ordered
results, rechecks their argv, timeouts, exit codes, and final
`git diff --cached --check`, and persists only the bounded structural result for
confirmation. Before validators start, the executor removes OpenCode home and
temporary state, unsets the provider credential from child environments, and
makes the parent process non-dumpable so target code cannot read the original
request or provider credential through `/proc`. OpenCode does not observe a
validation failure, issue a second model request, or repair the patch. Iterative
test-feedback repair is a possible future feature, not current behavior.

Each `allowed_commands` item contains an exact `argv` list and an explicit
whole-second or whole-minute timeout. Legacy command strings, shell executables, generic command dispatchers,
coding-agent re-entry, empty or multiline arguments, and per-command timeouts above the overall
execution timeout are rejected. Git is reserved for the final command, which must be exactly
`["git", "diff", "--cached", "--check"]`.

The published generic executor contains Go 1.25.12, OpenCode 1.18.2, Git, and CA
certificates. It does not include `make` or repository-specific development tools.
Configure only validators whose executables exist in the selected image. A missing
executable produces a bounded terminal failure and no actionable Fix PR preview.
The immutable image supports Go validators such as `go test`, `go vet`, and
`go version`. `GOTOOLCHAIN=local` prevents runtime toolchain downloads. The image
retains UID/GID 65532, the `/usr/local/bin/fixexecutor`
entrypoint, the fixed provider environment and output-leak protections, and the
same runtime security contract. The credential-free image fixture proves patch
generation, Go validation, result reconstruction, and
`git diff --cached --check`; it does not make a live provider claim.

The Helm values under `agentSandbox` must exactly match the project timeout,
turn, file, output, command, and provider settings. Deployed configurations
require an immutable executor image digest, explicit execution namespace,
workload ServiceAccount with token automount disabled, and non-empty secure RuntimeClass. Public
repositories only are supported because no Git credential enters the Sandbox.

Production Sandboxes request `RuntimeDefault` AppArmor and seccomp at both Pod
and container scope. There is no project, Helm, environment, or request field
that can disable AppArmor or select `Unconfined`. The Docker Desktop kind
evaluation omits AppArmor only through an internal Go test capability and does
not validate AppArmor enforcement or hostile-code isolation.

See [Kubernetes operator reference](kubernetes-reference.md#agent-sandbox-fix-runtime).
