# Changelog

All notable changes to the Aster engine are documented here. The
engine follows [Semantic Versioning](https://semver.org): consumers pin it via
`uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@<ref>`,
and the pinned ref controls both the workflow and the engine code it builds.

What bumps what:

- **MAJOR**: removing or renaming a `project.yaml` field, changing a reusable
  workflow input contract, or breaking the published data JSON schema.
- **MINOR**: a new optional config field, tool, or feature with safe defaults.
  Internal cache-version bumps (which force re-analysis on upgrade) are at least
  minor.
- **PATCH**: bug fixes, prompt tweaks, performance.

See [the release guide](docs/releasing.md#versioning) for the release process and
how to pin a consumer to a reviewed version.

## [Unreleased]

## [0.9.0-rc.7] - 2026-08-19

### Fixed

- **Fix generation was usually rejected on any branch but the repository
  default.** Resolving the generation base always read the repository's default
  branch, so a failure on a release branch resolved a base on `main` and the
  source compatibility check refused the branch mismatch before it ever compared
  commits. The only failure that slipped through was one whose commit happened
  to be the default branch's current head. The base is now resolved from the
  failure's own branch, and the pull request opens against that branch instead
  of the default one. Branch names arriving from build metadata are screened for
  unsafe ref syntax and escaped before they reach the API path.

  Rejections are also legible now. The source checks attach stable reason codes
  (`source_branch_unknown`, `source_revision_diverged`, `source_changed`). A
  rejected preflight returns the code in the `X-Analysis-Chat-Reason` header
  with a human-readable message in place of a bare "invalid analysis chat
  request", and an error raised once a chat stream is already running carries
  the same code in its SSE payload. The underlying cause is no longer dropped
  when preflight fails, and 4xx rejections are logged server-side alongside 5xx
  failures, so a refusal can be diagnosed from the server rather than guessed at.

  **Breaking (internal Go API).** `ghpr.Client.ResolveBase` takes a branch
  argument; an empty string keeps the previous default-branch behavior.
- **A dismissed pattern that aged out could not be restored.** Dismissing a
  recurring pattern retains the marker deliberately, because correlation can
  miss for a single pass and dropping the marker would return the pattern to the
  active view unbidden. But the overview reads only the active recurring set, so
  a dismissal whose pattern left that set stopped being shown there. A pattern
  whose lifecycle moved to recovered, observing, or verified fixed still offered
  Restore on its own banner; one that aged out entirely had no Restore path
  anywhere. The dismissed-patterns disclosure now lists both and offers Restore.
- **Semantic text and status colors rendered as default body text.** MUI 9
  resolves `color` as a styled variant rather than a system prop, so a dotted
  palette path such as `color="text.secondary"` or `color="error.main"` silently
  emitted no CSS. Components now use the canonical variant names, and an ESLint
  rule rejects literal dotted palette paths in the `color` prop so the failure
  mode cannot return silently.
- **Runtime trends were not anchored to run history.** The test detail page
  rendered the trend charts above run history, displacing the primary content,
  and the job detail page rendered them outside its run-history rail. Both now
  render directly beneath run history.
- **Docs and comments understated the engine's unattended GitHub writes.**
  Several places called the optional bot comment on new pull requests the
  engine's "only unattended GitHub write", and the README, the Pages guide, and
  the issues guide said a GitHub Pages deployment performs no GitHub writes at
  all. Both claims are wrong: the scheduled pass also comments on, and when
  configured closes, issues it already tracks whose finding has recovered, and
  the bot comment runs in the fetch step, so it works on Pages whenever
  `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` are supplied. The affected
  reference, example config, guides, and Go doc comments now state the accurate
  claim. Both write paths remain opt-in and off by default.
- **`-skip-side-effects` was described in terms of writes the pass no longer
  makes.** The flag help, `runSideEffects`, and the reference all enumerated
  "issue filing" and "fix PRs", which the scheduled pass stopped doing when
  unattended filing was removed. They now name what it actually suppresses:
  notifications, tracked-issue recovery, and pull request comments.
- **GitHub App permission guidance disagreed with itself.** The chart values and
  the reusable workflow input said the commenting App needs `issues:write` and
  nothing else, while the reference correctly notes that a private
  `branding.source_repo` also needs pull requests read-only. Both now match the
  reference.

  Those last three items are documentation and comments only, with no behavior
  change.

## [0.9.0-rc.6] - 2026-08-19

### Added

- **Optional bot comment on newly opened pull requests.** A contributor had no
  way to learn the dashboard was triaging their pull request or where the
  evidence lived. Enabling `pull_requests.comment` posts one comment per newly
  opened pull request linking to its triage page. It is the only unattended
  GitHub write that contacts a contributor's pull request, so it is off by
  default, and it stays in dry run, logging the exact body and posting nothing,
  until `dry_run` is explicitly `false`. `max_per_pass` bounds write attempts per
  pass and defaults to 10. `comment.enabled` without `pull_requests.enabled`
  fails validation.

  Posting authenticates as a GitHub App through `ASTER_APP_ID` and
  `ASTER_APP_PRIVATE_KEY`, so the comment comes from a bot account rather than a
  person. The App needs Issues read and write on `branding.source_repo`, since
  pull request comments go through the issues API, plus Pull requests read-only
  if that repository is private. Dry run takes the same path and stops only at
  the write, so it resolves the App identity too and reports the credentials as
  missing without them. Enabling commenting with no credentials mounted logs a
  warning and leaves the rest of the pass unaffected.

  Several rails bound the blast radius. A watermark recorded on the first pass
  makes every pull request older than it permanently ineligible, so enabling the
  feature never backfills. Drafts never reach triage, and a pull request opened
  by the commenting App itself is filtered out when candidates are selected.
  Before each write the engine re-reads the pull request and skips it unless it
  is still open, is still not a draft, carries no comment from the bot already,
  and is not one of Aster's own fix pull requests, which are opened under a
  different credential and so are recognized by their marker rather than by
  author. That final read rather than local state is authoritative, so a lost
  state file cannot double-post, and a read that cannot prove the absence of an
  existing comment fails closed. A pull request whose posts fail three times is
  abandoned. If the triage listing was truncated, commenting is refused outright
  rather than risk linking to a page that was not published. State lives in
  `pr_comment_state.json`, which is excluded from the published output.

- **Runtime trends on job and test detail pages.** Both pages showed one run at a
  time or a single mean duration, so a gradual slowdown was invisible. They now
  chart runtime across the current fetch window with median, p95, a direction
  over the window's halves, and outlier flagging on the latest run. Computed in
  the browser from data already fetched, so no new configuration and no new
  published fields.

- **The dashboard names the upstream repository when a dependency causes a
  failure.** A failure rooted in a dependency reported that fix investigation was
  unavailable and dropped the paths it had identified, which read as a dead end.
  Analyses and causal groups now carry an optional `cause_location` naming the
  owning repository, linking it when it is on GitHub, and listing the file paths
  the model identified, explicitly marked unverified because they were not read
  from that repository. A causal group's identity covers `cause_location` only
  once one is present, so causal groups built from cached analyses keep their
  identities and their remediation-investigation bindings on upgrade. A later
  fresh analysis that does name a location changes that group's identity, which
  orphans the group's existing binding and re-establishes it on the next
  investigation. Analysis identity itself does change on upgrade, so an
  analysis-chat Fix or an unconfirmed Fix preview started before the upgrade is
  rejected as stale and has to be started again. The analysis cache is
  unaffected: the critique version is unchanged, so nothing is re-analyzed.

- **Recurring failures keep a durable memory across build windows.** A cause that
  disappeared from the window and came back was investigated from scratch at full
  model cost, and its earlier verdict was lost. A private ledger now records each
  cause's signature, first and last sighting, and occurrence count, and reuses a
  previous terminal verdict when the frozen-input cache misses. Only verdicts that
  recurrence cannot contradict are reused, each at most three times; an actionable
  or insufficient-evidence verdict is always re-derived. Causal groups publish
  their `signature`, deliberately excluded from the identity hashes so the ledger
  cannot churn pattern identity. The ledger is stored in `recurrence_ledger.json`
  and is excluded from the published output.

- **Configurable attention thresholds.** A new optional `attention` section in
  `project.yaml` exposes two knobs that were previously fixed. `persistent_after`
  replaces the hardcoded consecutive-failure count of 3 that produces a
  `persistent` classification; it defaults to 3, and changing it moves the
  published classification, the flakiness report sections, the pull request
  attribution `known_flake` baseline, and notification eligibility together.
  `attention.low_pass_rate` adds a separate, opt-in selection rule that surfaces
  a test whose pass rate over the window falls strictly below a configured
  cutoff, so `threshold: 1.0` surfaces any test that failed at least once. The
  rule never changes a test's `classification`. It publishes a new
  `low_pass_rate` array in `flakiness.json`, whose entries carry their own
  `window_runs` and `pass_rate` because `recent_runs` can narrow the measurement
  below the window the entry's `fail_rate` covers, and the overview renders it
  as its own Needs Attention group from the item budget the recent, persistent,
  and flaky groups leave behind. Omitting the section leaves the rendered
  dashboard unchanged; `flakiness.json` gains an always-present `low_pass_rate`
  array that is empty when the rule is off, matching how the report's other
  sections are published. A `min_runs` guard, defaulting to 5, keeps a single
  failure out of two runs from being treated as signal. See
  [attention thresholds](docs/project-configuration.md#attention-thresholds).

  The notifier and issue recovery reconciliation no longer re-apply a literal
  threshold of 3 on top of the report's `persistent_failures`. Re-checking it
  would have silently dropped email for a consumer that lowered
  `persistent_after`, and dropped a still-failing test from the active issue key
  set, which recovery would then read as recovered and close. The consecutive
  failure count handed to the AI analyzer is now computed from the runs
  themselves rather than read off `persistent_failures`, so the engine-owned
  transient critique keeps seeing the true streak whatever a project configures.

### Changed

- **A new brand mark and color ramp.** The mark was assembled from two stock
  icons and blurred into an indistinct shape at favicon size. It is replaced by a
  single custom mark carrying a violet-to-pink ramp, and the docs identity and
  the app's blue primary are unified on that same violet. Status colors are
  unchanged, and a test now pins them so the brand ramp cannot drift into them.

- **Causal groups read as separate causes with an explicit Fix action.** Multiple
  causes ran together visually, and the control that routes to Fix was styled as a
  monospace chip that read like a data token rather than something to click. Each
  cause is now a titled card, and the routing control is a labeled button naming
  the humanized test rather than the raw JUnit name. A remediation state that is
  off for the whole deployment is now visually distinct from one that does not
  apply to a single cause.

### Fixed

- **Dismissing a pattern is reachable again on causal-group results.** Two
  independent gates, one in the dashboard and one in the server, each hid or
  refused dismissal for any pattern carrying a recurrence classification, which
  every published causal-group result does. Acknowledging such a pattern removed
  it from Needs Attention while its detail page still showed it as undismissed,
  and restoring it was impossible. Dismissal is now gated separately from
  drafting an issue or a fix, so it stays available where drafting is not, and
  restoring no longer requires the pattern to still be published, which used to
  strand a dismissal once the pattern aged out.

## [0.9.0-rc.5] - 2026-08-18

### Removed

- **Unattended issue filing and fix-PR generation.** Nothing creates a GitHub
  issue or pull request on a schedule any more. Aster's promise is guarded,
  maintainer-controlled next steps, and a background pass that opened issues and
  draft pull requests without anyone reviewing the finding first contradicted it.
  Filing an issue and proposing a fix are now exclusively authenticated dashboard
  actions, reviewed and confirmed by an admin. The scheduled pass keeps one
  narrow write a maintainer cannot reasonably perform by hand: commenting on and
  closing issues it already tracks whose finding has recovered.

  **Breaking for consumers.** Three changes need a consumer edit:

  - The reusable workflow no longer accepts the `FIX_TOKEN` or `ISSUE_TOKEN`
    secrets. A consumer `deploy.yml` that passes either one fails with
    `Invalid input`; delete those lines. Consumers using `secrets: inherit` need
    no change. A GitHub Pages deployment now performs **no** GitHub writes at
    all, so `issues.enabled` and `ai.fix_prs.enabled` have nothing to act on
    there. Both features require the Kubernetes-native server.
  - `FIX_TOKEN` is gone entirely. On Kubernetes, the server's `BOT_TOKEN`
    performs issue and fix-PR writes, and the worker's `ISSUE_TOKEN` performs
    issue recovery. `FIX_TOKEN` is also no longer a fallback for read-only
    GitHub source access; set `GITHUB_READ_TOKEN` if you relied on that.
  - `issues.max_new_per_run`, `ai.fix_prs.max_new_per_run`, and
    `ai.fix_prs.dry_run` are removed from `project.yaml`. They only bounded the
    scheduled generation that no longer exists, and `project.yaml` is decoded
    strictly, so leaving them in place fails validation.

  The `automatic_fix_prs` follow-up component is gone from the fetch status. A
  status file written by an older engine still loads.

- **The `persistent` issue trigger no longer files issues.** The dashboard's
  File issue action builds specs for systemic patterns and individual builds,
  never for persistent test failures, so the scheduled pass was the only thing
  that created them. The trigger stays in `project.yaml` and keeps scoping
  recovery: leave it enabled and persistent issues an earlier version filed
  still get their recovery comment and close. The fetcher also still computes
  which persistent findings are active, so no still-failing issue is closed
  early.

### Fixed

- **Concurrent issue-state writes no longer lose a filing.** The server files
  issues and the worker recovers them against the same `issue_state.json`, and
  each ran an unlocked load, mutate, and save. A worker pass that loaded before
  a filing could save over it, dropping the tracking entry so that issue was
  never closed on recovery. Both sequences now hold an exclusive lock.
- **Scheduled pods no longer request Fix authority.** With `ai.fix_prs.enabled`
  the Helm chart gave the worker and fetcher the `remote-fixer` image, Agent
  Sandbox environment, and the Sandbox client identity. Fix generation is
  server-only, so those pods no longer carry that access.
- **Filing an issue honors `issues.triggers`.** The File issue action always
  used the `patterns` trigger, so a project that disabled it could still file a
  pattern issue that scheduled recovery would never close.
- **The Helm chart supplies `GITHUB_READ_TOKEN` without `ai.enabled`.** The
  worker and fetcher rendered the read token only inside the AI block, and the
  read-token Secret was gated on AI or server actions. Pull request triage reads
  GitHub with no model calls, so `pull_requests.enabled: true` with
  `ai.enabled: false` had no chart value that reached the fetcher, leaving it to
  read anonymously at 60 requests per hour until triage silently stopped
  updating. `ai.githubReadToken` and `ai.githubReadTokenSecretName` now apply
  independently of `ai.enabled`. Renders with `ai.enabled: true` are unchanged.
- **Pull request attribution no longer depends on `source.include_presubmits`.**
  Attribution reused the flakiness report the same pass produced, which included
  presubmit history whenever that toggle was on. A `known_flake` verdict could
  therefore rest on other pull requests' builds rather than on the base branch,
  so the same failure was attributed differently depending on an unrelated
  dashboard setting. Flakiness for attribution is now recomputed over base-branch
  jobs only, and a verdict is identical either way. The toggle still enlarges
  each fetch and any enabled analysis, so `aster onboard doctor` now warns when
  it is on and reports when the optional triage view is unconfigured.
- **A lone peer pull request no longer preempts base-branch evidence.** A
  `widespread` verdict needed just one other open pull request failing the same
  job and test, so two pull requests citing each other were mutually excused and
  the failure dropped out of escalation. It now requires at least two other open
  pull requests, and cross-pull-request matching keys on the base branch as well
  as job and test name, because one job often runs on several release branches. A
  single peer is now recorded as evidence on whichever verdict does apply, except
  under `pre_existing`, which is decided before peers are consulted.
- **Doctor validates the OAuth callback for every admin-gated server feature.**
  `aster kubernetes doctor` checked `server.actions.oauth.redirectUrl` only when
  `server.actions.enabled` was set with an explicit `mode: oauth`. A chat,
  remediation, or escalation deployment therefore passed with a callback that
  would fail sign-in at runtime, as did any deployment leaving `mode` at the
  chart's `oauth` default. The origin security check in `aster onboard doctor`
  likewise ignored remediation and escalation. Both now cover every feature that
  signs an admin in.
- **The escalation panel no longer refetches without bound.** Its effect depended
  on an object rebuilt on every render, so a pull request failure page polled
  continuously instead of on an interval. It is now keyed on a stable subject.

### Added

- **Shared pull request failures are published and can be diagnosed.** When the
  same test failed across several open pull requests, triage detected the
  correlation and then offered nowhere to investigate it: the grouping was
  recomputed each pass and discarded, and a `widespread` verdict pointed at a
  peer pull request whose page pointed back. Each pass now writes
  `pull-request-failures.json`, keyed by the base branch, job name, and test name
  attribution already correlates on, with a stable `id` that survives pull
  requests joining and leaving. A failure is published as shared at two or more
  open pull requests, deliberately lower than the three a `widespread` verdict
  needs, because this view answers what is hitting several pull requests rather
  than whether one pull request is at fault. Clustering costs no model calls and
  ships on both deploy paths, so GitHub Pages gets the aggregate view with no
  server.

  On the Kubernetes server, one on-demand analysis can be run per shared failure
  under `POST /api/shared-failures/{id}/escalation`, reported by a new
  `shared_failure_escalation` capability. It is offered only when no member can
  already be analyzed from its own pull request, so there is never a second, more
  expensive path to the same answer, and both escalation kinds share one analysis
  slot. It runs under a new `sharedfailure` prompt module with no diff and its own
  cache namespace, and does not claim the affected changes are independent,
  because correlating on base branch, job, and test cannot establish that. The
  existing `server.pullRequestEscalation.enabled` value turns it on; there is no
  separate switch. A shared failure analysis is served only while it still
  describes the build a new request would read, so a superseded result is replaced
  rather than kept as history.
- **Pull request triage reports a missing GitHub read token.** The fetcher logs
  one startup warning when triage is enabled with neither `GITHUB_READ_TOKEN`
  nor `GITHUB_TOKEN` set, and `aster onboard doctor` reports the same gap as a
  `pull request triage credential` warning. Both name the anonymous 60 requests
  per hour ceiling that one triage pass can exhaust. The credential is now
  documented in the pull request triage configuration reference and in
  troubleshooting.
- **`server.pullRequestEscalation.enabled` Helm value.** On-demand analysis of a
  pull request failure deterministic triage could not explain had no chart value,
  so a Kubernetes deployment could not turn it on without hand-editing the server
  environment. It defaults to `false`, requires `ai.enabled` and
  `pull_requests.enabled`, and enables no GitHub writes and no `BOT_TOKEN`.
  `PULL_REQUEST_ESCALATION_ENABLED` is now reserved by the chart. `aster onboard
  doctor` gains a `Kubernetes pull request escalation` check for the
  preconditions the chart cannot see, including whether a GitHub read token
  actually reaches the server rather than leaving it on anonymous reads.

### Changed

- **The Agent Sandbox client ServiceAccount splits in two.** One identity used to
  back the server, the watch worker, and the cron fetcher, which made the subject
  of the Fix Sandbox RoleBinding the same Kubernetes object as the identity the
  scheduled causal critic and analysis shadow pods run under. The chart's
  mutual-exclusion guards already prevent Fix from being enabled alongside either
  scheduled feature, so no released configuration granted a scheduled pod Fix
  authority. The shared name was still the wrong shape: with
  `agentSandbox.rbac.create: false` an operator wiring RBAC out of band has one
  name to bind, so a Fix grant made for one release silently covers a later
  critic release running under the same name.

  `agentSandbox.rbac.clientServiceAccountName` is replaced by
  `agentSandbox.rbac.fixClientServiceAccountName` (server, Fix Sandboxes) and
  `agentSandbox.rbac.scheduledClientServiceAccountName` (worker and fetcher,
  critic and shadow Sandboxes). The default names change from
  `<fullname>-agent-sandbox-client` to `<fullname>-agent-sandbox-fix-client` and
  `<fullname>-agent-sandbox-scheduled-client`. A values file still setting the old
  key is rejected by the schema, and a render whose two identities resolve to the
  same name is rejected outright.

  **Upgrading.** With `rbac.create: true` the upgrade creates the new
  ServiceAccount and prunes the old one, and nothing else is required. With
  `rbac.create: false` the two keys must name two **distinct** externally managed
  ServiceAccounts. Do not point `scheduledClientServiceAccountName` at the
  ServiceAccount that previously served both roles: it still carries the
  out-of-band Fix RoleBinding, which would hand scheduled pods the exact Fix
  authority this split removes. Create a fresh scheduled ServiceAccount, or drop
  the Fix binding from the old one before any scheduled workload starts.

  The upgrade is disruptive to Sandbox operations already in flight. RoleBinding
  subjects and admission policy requesters switch to the new names as soon as the
  release applies, while running pods keep the ServiceAccount token they started
  with, so an active Fix, critic, or shadow Sandbox is denied until its pod is
  replaced. Upgrade when no Sandbox operation is in progress.

- **`issues.Manager` splits into `File` and `Recover`.** Each caller used exactly
  one half of the old `Reconcile`, and merging them meant the dashboard action
  passed options that only the scheduled pass read, and vice versa. `File` adopts
  or creates issues for the given findings; `Recover` comments on and closes
  tracked issues absent from the findings this run re-evaluated.

## [0.9.0-rc.4] - 2026-08-18

### Removed

- **Closed-loop remediation and post-merge Prow verification.** The engine no
  longer tracks a dashboard-created pull request through presubmit, pre-merge
  verification, post-merge observation, and same-cause recurrence. The
  `remediation` lifecycle package, its Prow catalog and evidence correlation,
  the remediation transition emails, and the dashboard lifecycle surfaces are
  gone. `remediations.json` is a retired public projection: a normal refresh now
  deletes it so an upgraded deployment cannot keep serving stale lifecycle data,
  and the Pages deploy strips it from a pre-fetched data directory. No
  `project.yaml` field changed, so a consumer needs no configuration edit.

### Fixed

- **Remediation and fix routing are reported per causal group.** A recurring
  pattern with several distinct causes previously showed one remediation verdict
  above the whole group list and pointed "Fix a specific failure" at a grid of
  affected builds without distinguishing which of them could actually support a
  Fix investigation. Each cause now carries its own state and routes to its
  representative analyzed failure, and causes that cannot produce a reviewable
  patch say so instead of offering an unusable path.
- **The overview sparkline renders every configured run.** Jobs whose recent
  history was shorter than the configured window dropped trailing runs from the
  reliability sparkline, so the overview could disagree with the job ledger.
- **OAuth failures are diagnosable and credentials reject stray whitespace.**
  Sign-in failures now report a specific cause instead of a generic error, and
  credential environment variables are sanitized once at startup so a token with
  a trailing newline fails fast with a clear message rather than as an opaque
  provider or GitHub rejection. `aster onboard doctor` checks the same thing.
- **Pull request escalation admission is bounded before resolution.** An
  escalation is admitted against a bounded queue, drained on shutdown, and
  bounded by the accepted escalation lifetime. A build with no finished metadata
  is refused, an escalation whose resolution was cut off is rejected rather than
  left pending, a failed escalation can be retried, and an oversized restored
  store is pruned so a restart cannot exceed the retention bound.
- **In-flight pull request escalations are awaited on shutdown**, so a server
  stop no longer abandons an escalation that was mid-flight.
- **Action request cleanup is single-flight**, removing a race that made CI
  flaky and could let concurrent cleanups interleave.

### Changed

- **Documentation consolidated.** The docs tree was reorganized around the
  current feature set and stale pages describing removed runtimes and the
  closed-loop remediation lifecycle were deleted. `README.md` now introduces
  Aster and its operating model directly.

## [0.9.0-rc.3] - 2026-08-17

### Removed

- **Orka and SRT runtimes.** The experimental Orka container/agent runtime and
  the Anthropic Sandbox Runtime local process sandbox are gone. Agent Sandbox is
  now the only coding-agent runtime. `analysisRuntime.type` accepts only
  `inprocess`, and `ai.fix_prs.agent_runtime.type` accepts only `agent-sandbox`,
  which is also the default. The removed `agent_runtime` fields are `model`,
  `network_domains`, and the Orka-only `agent_ref`, `api`, `namespace`,
  `version`, and `retries`. `allow_bash` now defaults to false and must be
  false, and `critique_retries` is pinned to zero.
- **Interactive source investigation.** Orka was its only backend, so the
  `ai.source_investigation` project block, the
  `server.chat.sourceInvestigation` chart values, the chat source-investigation
  endpoints, and the dashboard panel were removed.
- **Agent prompt authoring mode.** `aster onboard` now offers only `handoff`
  (the default, which writes a reusable skill and reviewable prompt for the
  operator's own coding agent) and `todo-template`. The `--prompt-agent-*`,
  `--prompt-orka-*`, `--prompt-network-domain`, and `--require-prompt-draft`
  flags were removed.

### Changed

- **Fix execution bounds have one source.** `max_turns`, `max_files`,
  `timeout`, `output_limit_bytes`, and `allowed_commands` are configured only in
  `project.yaml` under `ai.fix_prs.agent_runtime`. The chart derives the workload
  environment and admission deadline from the inlined `project.config`, so the
  matching `agentSandbox.fixRuntime` values were removed. A stale copy now fails
  the schema; `upgrade.sh` strips those keys from candidate values during an
  upgrade.
- **Quality benchmarks relocated.** The opt-in model-quality benchmarks moved
  from `backend/internal/e2e` to `backend/benchmarks`. `internal/e2e` now holds
  only the hermetic pipeline regression test.

### Added

- **Pull request failure triage.** The dashboard gained a per-open-pull-request
  view of presubmit results, including attribution against observed baselines
  and reporting when a failure sits in changed code.
- **Opt-in AI escalation for unexplained pull request failures.**

### Fixed

- **Conversation-scoped chat-to-fix eligibility.** Chat fix evidence is scoped to
  the conversation rather than a single turn, and permanent ineligibility is
  reported first.
- **Durable file-link verification.** Transient GitHub failures no longer drop
  published file links.
- **Release image identity.** Published images keep the title declared in their
  Dockerfile stage instead of inheriting the repository name, so release image
  verification passes. The Agent Sandbox analysis executor now publishes to its
  own repository rather than over the fix executor.

## [0.9.0-rc.2] - 2026-08-15

### Added

- **Exact-version consumer onboarding.** The setup skill resolves and verifies
  an immutable release before generating consumer configuration, including the
  matching nested Go module version.
- **New-user guidance.** The documentation now makes Prow and TestGrid
  prerequisites explicit and presents one exact-version onboarding path.

### Changed

- **Aster product identity.** The repository and public documentation now use
  Aster, Automated Signal Triage, Explanation, and Remediation.
- **Paired Go release tags.** Releases publish matching root and `backend/`
  module tags, and verify both tags point to the same commit before publishing
  release artifacts or moving a stable alias.
- **Dependency-aware CI.** Change classification routes affected checks while
  preserving cross-contract coverage, with Helm static validation separated
  from the kind-based lifecycle test.
- **Documentation organization.** Historical planning documents were removed,
  and active experimental runtime references now live under maintainer
  documentation.
- **Deterministically verifiable remediation targets.** New remediation model
  output is limited to supported target kinds that the engine can verify.
  Previously stored payloads with removed kinds still decode but fail closed
  during deterministic verification.

## [1.0.0-beta.7] - 2026-07-28

### Fixed

- **Fail-fast Orka project setup.** Orka container analysis validates the
  complete project bundle source before Prow discovery and treats later source
  failures as systemic, preserving public output, traces, cache, chat, action,
  notification, and remediation state instead of publishing a degraded pass.
- **Orka project ConfigMap materialization.** The Helm CronJob now copies the
  allowlisted project configuration, prompt, and optional skill recipes from
  the ConfigMap volume into a read-only runtime mount before Orka container
  analysis starts, preserving strict symlink rejection in the bundle loader.

## [1.0.0-beta.6] - 2026-07-27

### Added

- **Recurring-pattern analysis chat backend.** Kubernetes-native analysis chat
  accepts owner-bound recurring-pattern references, validates the complete
  published pattern hash, snapshots the pattern across restarts, and exposes
  read-only artifact tools for at most the three newest affected builds under
  build-qualified paths. Pattern findings remain eligible for the existing
  chat-to-fix flow, while test-analysis correction promotion stays disabled.
  Recurring-pattern cards expose the existing **Chat with agent** experience
  with pattern-specific prompts and build-qualified evidence links.
- **Analysis-chat fix context bridge.** Kubernetes-native deployments with both
  analysis chat and write actions can generate an existing fix preview from one
  selected evidence-backed assistant response. The server reconstructs bounded
  context from the owner-bound session, validates the current analysis and target
  recurring pattern, optionally includes one successful verified source
  investigation, and never forwards the complete transcript. Eligible completed
  responses expose a **Use this finding in a fix proposal** control with an
  explicit context-review step, the existing draft preview, and final GitHub
  confirmation.
- **Read-only source investigation for analysis chat.** Kubernetes-native chat
  can opt into owner-bound Orka read-only agent Tasks pinned to the selected Prow
  build's exact repository commit. Requests persist across replicas, support
  reconnect and cancellation, reject workspace changes, and return only bounded
  source citations verified against the pinned revision. Completed chat answers
  expose dashboard controls for progress, reconnect, cancellation, and verified
  source findings. The capability uses a dedicated Task-only ServiceAccount and
  no GitHub write token.
- **Authenticated analysis-trace console.** Server mode now exposes the private
  trace snapshot through admin-gated filtered and download endpoints and a
  dedicated operator page. Static Pages remains unchanged, and direct
  `/data/ai_traces.json` access still returns 404.
- **Private in-process analysis traces.** Each AI pass writes a bounded,
  sanitized `ai_traces.json` operational snapshot with model request metadata,
  response IDs, usage, tool names, compaction, critique, and finalization
  events. The server denies the file under `/data`, and Pages strips it before
  publication.
- **Regular-harness Responses API.** In-process analysis can select `ai.api: responses`, preserves reasoning items across function calls, and sends `store: false`.
- **Orka fix generation runtime.** Fix PRs can opt into a generation-only Orka
  Agent Task while keeping base pinning, diff reconstruction, review,
  verification, previews, credentials, and PR creation inside the engine. The
  chart automatically selects a git-capable engine image for this mode. The
  runtime is documented and tested with Orka Agents that select the OpenCode
  CLI while preserving the generic `orka` dashboard backend.
- **Experimental Orka container analysis runtime.** Kubernetes Helm deployments
  can opt into `analysisRuntime.type: orka-container` with `mode: cron`. One
  content-addressed `type: container` Task per failure runs the current
  dashboard-owned `FailureAnalyzer`; the in-process runtime remains the default
  and only recommendation. The adapter includes a dedicated analyzer image,
  Task-only RBAC, immutable sanitized bundles, framed results, encrypted raw
  cache and trace state, evidence-coverage round-trip protection, bounded
  bundle and terminal Task cleanup, failed-Task trace retention, explicit CPU
  pool placement, and an isolated kind lifecycle test. It has no Pages
  or watch-mode support, and its interfaces may change without notice.
- **SMTP email notifications.** Consumers can configure persistent-failure,
  changed-error, and recovery email alerts under `notifications.email`. SMTP
  passwords are supplied through the `EMAIL_SMTP_PASSWORD` deployment secret;
  STARTTLS is the default transport. Kubernetes-native deployments can opt into
  inert email links that open the authenticated issue or fix preview flow for
  systemic recurring patterns. Draft generation can run asynchronously with
  persisted 24-hour review requests and draft-ready email links.

### Removed

- **Patched Orka AI analysis runtime.** The generic `type: ai` worker path stays
  removed with its producer, ingestor, artifact Tool service, Provider proxy,
  worker patch, compatibility versions, and alternate submission and validation
  protocols. Repeated parity trials showed model-run variance rather than a
  stable runtime advantage. The optional container runtime does not restore any
  of those components or add Orka-specific fields to the private trace schema.

- **Slack webhook notifications.** `SLACK_WEBHOOK_URL` and Slack Block Kit
  delivery are removed. Consumers that need notifications must configure the new
  email transport before upgrading. This is a breaking reusable-workflow secret
  change.

- **Self-improving skills (`ai.suggest_skills`).** The opt-in feature that
  auto-drafted `skills/<id>.yaml` recipe PRs for uncovered systemic patterns is
  removed, along with its `SKILL_TOKEN` workflow secret. Authoring recipes by
  hand under `skills/*.yaml` is unchanged; only the auto-suggestion is gone.
  Removing the `ai.suggest_skills` field is a breaking config change.

## [1.0.0-beta.5] - 2026-07-09

### Added

- **Kubernetes-native deploy mode.** The engine now runs in-cluster as a strict
  superset of the GitHub Actions + Pages path, so it can sit next to a private
  in-cluster inference stack. A new `cmd/server` serves the exact same
  `/data/*.json` contract the static site reads, plus a `/api/capabilities`
  descriptor the frontend probes to light up server-only features; with no
  descriptor the frontend stays in read-only static mode, so one build serves
  both targets. A `cmd/worker` runs a continuous watch loop (incremental every
  few minutes, full rediscovery hourly) writing to a shared volume the server
  reads. Ships as a single container image (fetcher + server + SPA) and a Helm
  chart (`deploy/helm/aster`: fetcher CronJob + server from a shared
  RWX volume). See [docs/kubernetes.md](docs/kubernetes.md) and
  [docs/server.md](docs/server.md).
- **Admin-gated on-demand actions** (server mode). Signed-in admins can, per
  systemic failure, file a GitHub issue, propose a draft fix PR, or mark a
  pattern resolved, reusing the same engines as the scheduled path. Two auth
  modes behind one seam: `oauth` (per-user attribution via a GitHub OAuth App,
  each admin's token held in an encrypted httpOnly session cookie) and `proxy`
  (an upstream SSO proxy authenticates; a bot token performs the write). Off
  unless configured; CSRF-guarded, admin-allowlisted, tokens never logged or
  served. The issue and fix actions are two-phase: a **preview** renders the
  exact issue or PR (and, for a fix, the diff) without posting, with an optional
  refine-by-prompt step, then a confirm posts the previewed draft.
- **Mark recurring patterns resolved.** A maintainer can mark a systemic pattern
  resolved (often it is fixed by a change in a repo the engine does not watch);
  it moves to a collapsed "Resolved" section and **auto-reopens** if the failure
  recurs on a build newer than the watermark recorded at resolution time, so a
  flake that comes back is never permanently hidden. State lives in
  `resolved.json`, served read-only.
- The Helm chart is now published on each release: a `v*.*.*` tag pushes it to
  `oci://ghcr.io/<owner>/charts/aster` (image pinned to the release)
  and attaches the packaged `.tgz` to the GitHub Release.
- `make dev-actions` previews the server-mode UI with admin actions enabled
  locally (local proxy auth, no OAuth setup), unlike the read-only `make dev`.
- Optional **agent-proposed fix PRs** (`ai.fix_prs`): after each fetch, for a
  systemic recurring pattern with a concrete remediation, the engine drafts a
  minimal code fix and opens a **draft PR** against the source repo via
  fork-and-PR. Off by default and heavily guardrailed: the target file(s) are
  chosen from the repo's **real file tree** so the model can't invent a path;
  **anchored search/replace** edits are applied only on an exact single match and
  bounded by `max_files`; each edited file is **parse-checked** (Go/YAML/JSON)
  and the fix is dropped if an edit broke it; a second LLM **review**
  (`critique_retries`, default 1) re-prompts on concrete defects and drops the
  fix if unresolved; draft-only; a dedicated `FIX_TOKEN` (a CLA-signed
  contributor PAT) authors the commit under that identity with a DCO
  `Signed-off-by`; idempotent marker dedup; and a `max_new_per_run` cap. A
  `dry_run` mode runs the full pipeline and writes proposed diffs to
  `fix_previews.json` without opening any PR. `fork: false` (default `true`)
  switches to a direct branch + same-repo PR for a source repo you own. See
  [docs/fix-prs.md](docs/fix-prs.md).
- Optional **self-improving skills** (`ai.suggest_skills`): after each fetch,
  the engine drafts a diagnostic skill recipe for any systemic recurring pattern
  that no existing skill covers, and opens a **draft PR** adding
  `skills/<id>.yaml` to the dashboard repo for review. Off by default. Reuses the
  configured AI provider to decide coverage and draft the recipe, validates the
  draft against the skills schema before proposing, and dedupes by a hidden
  marker. Needs a `SKILL_TOKEN` secret. See
  [docs/skills.md](docs/skills.md#auto-suggesting-recipes).
- New `aster onboard` subcommand scaffolds a new dashboard from a testgrid
  dashboard name or a storage bucket. It verifies discovery finds jobs, infers
  `categories` from the job names, and writes a ready-to-review scaffold
  (`project.yaml`, both workflows, a `prompts/system.md` draft, a `CHECKLIST.md`),
  validating the generated config against the engine's own loader before writing.
  When AI creds are set it drafts `prompts/system.md` from the source repo's own
  docs; otherwise it writes a stub. Pass `-open-pr` to open a scaffold PR instead
  of writing locally, and `-mode k8s` to also scaffold a `deploy/` folder. See
  [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md#interactive-wizard).
- Optional **auto-filing of GitHub issues** for the dashboard's highest-signal
  findings: systemic recurring patterns and persistent failures (>=3 consecutive
  runs). Off by default; enable with an `issues:` block plus an `ISSUE_TOKEN`
  secret. Each finding maps to one issue, deduped by a hidden marker via local
  state plus an eviction-proof repo-side search. Recovered findings get a
  "recovered" comment. See [docs/github-issues.md](docs/github-issues.md).
- New internal `ghpr` helper extracts the one-commit "open a PR from a file-set"
  flow (GitHub Git Data API) shared by onboarding, skill suggestions, and fix
  PRs, with seams for draft, labels, commit author, and DCO sign-off.

### Changed

- **Breaking: AI analysis now requires an explicit endpoint and model.** The
  engine no longer defaults to GitHub Copilot when `ai.endpoint` / `ai.model` are
  unset. When AI is enabled, configure both in `project.yaml` or via the
  `AI_ENDPOINT` / `AI_MODEL` env vars; otherwise the fetch fails fast with a
  clear error. This makes the engine fully provider-agnostic with no opinionated
  default.
- **Breaking: config surface trimmed.** `ai.pattern_analysis`, the critique
  enable flag, and several low-value `project.yaml` fields were removed; critique
  and the investigation floors are now always-on engine defaults rather than
  per-project toggles. Consumers that set the removed fields must drop them, as
  the loader rejects unknown fields.
- `AI_TOKEN` no longer falls back to `GITHUB_TOKEN`; it is the credential for the
  configured chat-completions endpoint and must be set explicitly to enable AI
  analysis. Deployed consumers already pass it and are unaffected; only local
  runs that relied on the implicit fallback now need it set.
- **Frontend redesign** with a new theme and command-band dashboard layout.
- **Fix-PR file selection is now agentic.** Instead of keyword ranking alone, the
  fix harness runs a bounded source-tree loop (grep/read the repo) to choose
  target files, grounded on the analysis's implicated files, and declines clearly
  when the fix belongs to an upstream dependency rather than inventing an in-repo
  edit.
- **Stronger critique gate** (forces re-analysis on upgrade). The deterministic
  judge now rejects a "transient" verdict on a test that has failed many
  consecutive builds, and an optional model-backed **semantic judge** runs after
  the deterministic pass. These bump the critique cache version, so existing
  analyses are re-evaluated once on the first run after upgrading.
- Consolidated getting started into a single path: removed `docs/quickstart.md`
  and made `docs/onboarding-a-new-project.md` the one entry point. The README is
  restructured and indexes every doc, and local-development setup moved to a new
  `docs/development.md`. Docs are now provider-agnostic.

### Fixed

- **Bounded recurring-pattern parsing.** Pattern responses now select one
  unambiguous schema-valid JSON candidate from fenced output, metadata wrappers,
  or trailing provider prose, reject partial or invalid-build verdicts with safe
  validation categories, and abort publication when pattern regeneration fails.
- **Safe Orka refresh publication.** Orka result API 401 and 403 responses now
  abort the refresh before public output or notifications, stop additional
  analysis scheduling, hide response bodies, and restore the prior AI cache and
  private trace files byte for byte.
- **Rotating Orka result credentials.** Result clients now reload file-backed
  credentials for every request, and Helm deployments can use projected
  ServiceAccount tokens for container analysis, fix generation, and source
  investigation while retaining explicit static Secret compatibility.
- Fix-review parsing now selects the final valid balanced JSON object instead
  of combining reasoning prose and code braces into one invalid payload.
- Analysis chat exposes a Helm-configurable per-turn timeout up to 30 minutes,
  while retaining the two-minute default for existing deployments.
- The Helm chart tolerates chmod-less RWX volumes (SMB/azurefile return EPERM on
  chmod; the mount's file mode governs readability), and gained `server.extraEnv`
  for injecting extra configuration.
- The server sets `Cache-Control` so browsers revalidate `/data/*` and
  `index.html` instead of serving a stale dashboard or SPA after a deploy.

## [1.0.0-beta.4] - 2026-06-26

### Added

- New job-level, cross-build **pattern analysis** (always on, no flag). After
  the per-failure analyses complete, for any job that failed in at least 3 recent
  builds the engine correlates one representative failure per failed build into a
  single verdict: do these failures share one root cause (a systemic, fixable bug
  surfacing as repeated "flakes") or are they genuinely independent? The specific
  failing test/spec may differ between builds; the pass weighs the underlying
  mechanism. Like artifact-tree seeding it is not configurable: self-gating (a
  no-op on a healthy dashboard) and cached, so it costs nothing until a job
  genuinely recurs, then one extra tool-free model call. It surfaces as a banner
  at the top of the job page, and the systemic verdicts are aggregated across all
  jobs into the landing page's **Needs Attention** box. See
  [docs/agentic.md](docs/agentic.md#pattern-analysis).
- Editing `prompts/system.md` now takes effect automatically: each analysis is
  fingerprinted with the prompt that produced it, and on the next run any failure
  whose prompt no longer matches is re-analyzed. No manual cache clear is needed.
  Re-analysis is incremental and failure-preserving (an old analysis stays
  published until its replacement succeeds), so results aren't lost while it
  catches up. The **Clear AI Cache** workflow remains available to re-baseline
  everything at once. Note: the first run after upgrading re-analyzes existing
  entries once (they predate the fingerprint), consistent with other
  cache-version bumps.

### Fixed

- Bucket-discovered jobs (`discovery.source: bucket`) now get a display title and
  category. They previously rendered as untitled cards under "Other" because the
  bucket path did not set `tab_name` or apply the project's `categories` rules
  (only the testgrid path did). The categorize logic is now shared across both
  discovery sources. The job-card title also falls back to the job name when no
  tab name is present.

### Changed

- The landing page's **Needs Attention** box is now collapsible, with its
  open/closed state remembered across visits, so a long alert list no longer
  pushes the job grid down the page.
- `storage.provider` is now required (no implicit `gcs` default), so the config
  is explicit about the backend rather than assuming a provider. Set
  `provider: gcs` for Google Cloud Storage. Consumers already setting a provider
  are unaffected.

## [1.0.0-beta.3] - 2026-06-25

### Added

- Storage is now pluggable so the engine no longer assumes Google Cloud Storage.
  A new `storage.provider` selects the backend: `gcs` (native GCS, the previous
  behavior) or `gcsweb` (any gcsweb HTTP gateway fronting a bucket, e.g. an S3
  bucket behind `gcsweb.<project>.io`). For `gcsweb`, set `storage.base` (the
  gateway) and optionally `storage.prow_base`/`storage.web_base`. Ranged reads
  are emulated for gateways without HTTP Range support.
- Pluggable job discovery via `discovery.source`: `testgrid` (default, the
  kubernetes/test-infra path) or `bucket`, which lists the artifact bucket's own
  `logs/` and `pr-logs/directory/` indexes and needs no job-config repo. Works
  for any Prow instance; optional `discovery.job_filters` scope by job-name
  substring. Together these let non-kubernetes Prow projects (e.g. Istio on S3)
  onboard with no engine changes.

### Changed

- **BREAKING (config):** the `gcs:` block is replaced by `storage:`. Migrate
  `gcs: {bucket: X}` to `storage: {provider: gcs, bucket: X}`. `testgrid.dashboard`
  is now required only when `discovery.source` is `testgrid` (the default).

## [1.0.0-beta.2] - 2026-06-24

### Added

- Release process: tag-triggered release workflow, semver tags, a moving
  `vMAJOR` alias, this changelog, and `docs/releasing.md`.
- Engine version is embedded at build time and logged at startup; an optional
  `min_engine_version` field in `project.yaml` warns when the engine is older
  than the config expects.
- Quickstart guide and a "Tuning by model tier" reference for the agentic loop.
- In-cluster self-hosted runner guide for private AI endpoints.
- AI analysis rendering: running builds show a yellow (not red) status dot;
  inline `code` spans render as monospace pills; and cited file paths link to
  their source. Source links are verified to exist at fetch time
  (`file_links` on each analysis) so a file in a different repo than the project
  is never turned into a broken link. Repo resolution is generic (project repo,
  Go vanity import via `?go-get=1`, or `owner/repo/path`) with no project- or
  ecosystem-specific knowledge in the engine. Inline links display just the
  filename, with the full path shown on hover.

### Changed

- **Single-pin engine reference**: the deploy workflow now builds the engine at
  the pinned workflow commit. The `engine-ref` input was removed. No action
  needed for consumers (none set it); `@main` callers are unaffected.

### Fixed

- Deep links no longer render a blank page on GitHub Pages (SPA fallback).
- Oversized junit failure messages and artifact-tree seeds no longer overflow
  the model context window on the first request.
- Slow chat endpoints no longer hit a fixed per-request HTTP timeout: each chat
  request is now bounded only by the per-failure `timeout` budget, so reasoning
  and self-hosted models whose decode exceeded the old 60s client cap complete
  instead of erroring out.
- A failure whose analysis could not complete (endpoint error, timeout, or a
  misconfigured run) now has its "AI analysis unavailable" summary refreshed on
  the next run instead of keeping the stale message. Errored failures are
  re-analyzed every run, so once the endpoint is healthy they converge to a real
  analysis; transient classifications and real summaries are still preserved.
