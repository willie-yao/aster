# Changelog

All notable changes to the prow-ai-dashboard engine are documented here. The
engine follows [Semantic Versioning](https://semver.org): consumers pin it via
`uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@<ref>`,
and the pinned ref controls both the workflow and the engine code it builds.

What bumps what:

- **MAJOR**: removing or renaming a `project.yaml` field, changing a reusable
  workflow input contract, or breaking the published data JSON schema.
- **MINOR**: a new optional config field, tool, or feature with safe defaults.
  Internal cache-version bumps (which force re-analysis on upgrade) are at least
  minor.
- **PATCH**: bug fixes, prompt tweaks, performance.

See [docs/releasing.md](docs/releasing.md) for the release process and
[docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md#versioning-and-pinning)
for how to pin a release.

## [Unreleased]

### Added

- **Encrypted container analysis state.** The experimental dashboard analyzer
  now seeds the one relevant cache entry into each immutable Task bundle and
  returns cache and content-free private trace deltas through an AES-GCM
  encrypted state marker. A dashboard-owned state store merges the authenticated
  delta into the existing atomic `ai_cache.json` and `ai_traces.json` files.
- **Immutable container analysis bundles.** The experimental dashboard-owned
  analyzer now receives its failure request, sanitized project config, prompt,
  and consumer skills from an immutable content-addressed ConfigMap. Tasks use a
  ConfigMap key reference, verify the full digest before materializing private
  temporary files, keep credentials in Secret references, reject credential-
  bearing endpoint URLs and unsafe YAML graphs, and bound the environment
  transport. Terminal
  result handling
  deletes private Task-scoped bundles immediately, while failed Task application
  leaves inputs for one batch-level GC pass to prune after 24 hours under
  least-privilege ConfigMap RBAC. Short-lived claims and resource-version deletes
  prevent GC from deleting a bundle concurrently adopted by another reconciler.
  Terminal cleanup is bound to the
  observed Task UID so delayed handling cannot delete a replacement Task input.
- **Dashboard container result framing.** The experimental dashboard-owned
  analyzer now emits a bounded base64 result marker on stdout. Mixed Orka Task
  logs are parsed for the last valid marker instead of treating the final line
  as JSON, with strict rejection of missing, malformed, oversized, conflicting,
  or empty results.
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
  chart automatically selects a git-capable engine image for this mode.
- **SMTP email notifications.** Consumers can configure persistent-failure,
  changed-error, and recovery email alerts under `notifications.email`. SMTP
  passwords are supplied through the `EMAIL_SMTP_PASSWORD` deployment secret;
  STARTTLS is the default transport. Kubernetes-native deployments can opt into
  inert email links that open the authenticated issue or fix preview flow for
  systemic recurring patterns. Draft generation can run asynchronously with
  persisted 24-hour review requests and draft-ready email links.

### Removed

- **Frozen Orka AI analysis backend.** The patched `type: ai` worker, Helm
  analysis selector, producer, ingestor, artifact Tool service, Provider proxy,
  compatibility workflow, manifests, private analysis manifest, and worker
  patch assets are removed.
  Failure analysis now always uses the dashboard-owned in-process analyzer. Orka
  fix generation and the isolated container lifecycle experiment remain.

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
  chart (`deploy/helm/prow-ai-dashboard`: fetcher CronJob + server from a shared
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
  `oci://ghcr.io/<owner>/charts/prow-ai-dashboard` (image pinned to the release)
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
- New `fetcher onboard` subcommand scaffolds a new dashboard from a testgrid
  dashboard name or a storage bucket. It verifies discovery finds jobs, infers
  `categories` from the job names, and writes a ready-to-review scaffold
  (`project.yaml`, both workflows, a `prompts/system.md` draft, a `CHECKLIST.md`),
  validating the generated config against the engine's own loader before writing.
  When AI creds are set it drafts `prompts/system.md` from the source repo's own
  docs; otherwise it writes a stub. Pass `-open-pr` to open a scaffold PR instead
  of writing locally, and `-mode k8s` to also scaffold a `deploy/` folder. See
  [docs/onboarding-a-new-project.md](docs/onboarding-a-new-project.md#fast-start-scaffold-it-with-onboard).
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
