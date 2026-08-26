# AGENTS.md

Guidance for AI coding agents working on Aster. See
[`README.md`](README.md) for the human-facing introduction and
[`docs/`](docs/) for deep dives.

## Project overview

Aster is the **reusable engine** for AI-powered Prow/TestGrid
dashboards. It is consumed by lightweight per-project repos (the
"consumers") via a reusable GitHub Actions workflow or Helm chart. The engine
repo holds all the code; consumer repos hold `project.yaml`,
`prompts/system.md`, and a small deployment file. See [`README.md`](README.md)
for the user-facing deployment choices.

## Product identity

Aster is written as a proper name. Its operating-model expansion is
**Automated Signal Triage, Explanation, and Remediation**. The product promise
is evidence-first failure analysis with guarded, maintainer-controlled next
steps. Do not describe Aster as autonomous repair, self-healing, or guaranteed
root-cause detection.

Use the opening of [`README.md`](README.md) as the source of truth for the
public tagline and product description, and [`docs/brand.md`](docs/brand.md) for
the mark, palette, and asset rules.

The data flow per scheduled deploy is:

```
Consumer workflow (cron)
   └─> Reusable workflow `.github/workflows/reusable-deploy.yml`
         ├─> Checks out engine + consumer side-by-side
         ├─> Builds backend/cmd/aster
         ├─> aster -project-dir=<consumer> -out=engine/frontend/public/data
         │     ├─> Loads consumer's project.yaml + prompts/system.md
         │     ├─> Discovers prow jobs from kubernetes/test-infra
         │     ├─> Fetches recent builds + JUnit XML from GCS
         │     ├─> Runs AI analysis per failing test (agentic, cached)
         │     └─> Writes manifest.json + dashboard.json + jobs/*.json + ...
         ├─> Builds frontend/ (Vite, with VITE_BASE_PATH for gh-pages subpath)
         └─> Deploys built site via actions/deploy-pages to consumer's GH Pages
```

## Repo layout

Grouped by role rather than alphabetically. Every `backend/cmd` and
`backend/internal` package appears here; `make check-repo-map` fails if this
list and the tree diverge.

```
backend/                         Go 1.25
  cmd/
    aster/                      Public CLI and one-shot pipeline; Pages and the k8s CronJob
    worker/                      Continuous in-cluster watch loop (k8s mode: watch)
    server/                      API server: /data/* read parity, capabilities, actions
    analysisexecutor/            Runs one file-backed OpenCode analysis workload
    analysisstager/              Copies one sealed analyzer snapshot into the workspace
    fixexecutor/                 Runs one credential-free Agent Sandbox fix workload
  internal/
    -- core pipeline (discover -> analyze -> write) --
    fetcher/                     Orchestration invoked by cmd/aster and cmd/worker
    prow/jobconfig/              kubernetes/test-infra job discovery + parsing
    prowbuild/                   Addresses Prow build artifacts over a storage.Backend
    storage/                     Pluggable artifact store (gcs / gcsweb / local)
    artifacts/                   Read-only Browser over one build's artifact tree
    junit/                       JUnit XML -> structured test cases
    aggregator/                  Per-job and per-test aggregate statistics
    patterns/                    Correlates analyzed failures across builds
    recurrenceledger/            Durable memory of recurring causes across build windows
    prtriage/                    Per-open-pull-request view of presubmit results
    prattribution/               Rules a pull request out of a failure from observed results
    prescalation/                On-demand AI analysis for unexplained pull request failures
    prcomment/                   Opt-in bot comment on newly observed pull requests
    output/                      Writes the JSON contract the frontend reads
    models/                      Shared wire-format types
    fetchprogress/               Persists safe aggregate fetch progress for operators

    -- analysis (the most active area) --
    ai/                          Model transports + the agentic tool-calling loop
      agentic.go                 The loop, finalize branch, floor gates
      toolloop.go                Tool dispatch and transcript management
      service.go                 Cache key, staleness, shouldReanalyze
      critique.go                Deterministic judge that gates drafts
      semantic.go                Semantic judge pass
      compose.go                 BasePrompt + consumer system.md + ResponseFormatFooter
      cache.go / cache_acceptance.go  On-disk cache and its acceptance floors
      evidenceplan/              Ranked evidence planning + deterministic repair
      tools/{filesystem,k8s,repotree}/  Function-calling tools exposed to the model
      skills/                    Diagnostic recipe registry (+ builtin/{prow,kubernetes})
      modules/universal/         Builds the per-failure seed prompt
      modules/pullrequest/       Seed prompt plus pull request change context
      modules/sharedfailure/     Seed prompt plus the cross-pull-request correlation
    aiusage/                     Private token usage and cost accounting
    agentanalysis/               Private experimental Agent failure-analysis adapter
    analysispublisher/           Namespace-local analyzer input Job lifecycle
    analysisexecutor/            File-backed OpenCode analysis executor
    analysisstager/              Credential-free analyzer workspace stager
    agentsandbox/                Business-neutral Agent Sandbox lifecycle contract
    modelprovider/               Shared non-secret provider and credential-mode contract
    analysisruntime/             Selects the failure-analysis runtime (in-process default)
    analysischat/                Bounded conversations about a published analysis
    corrections/                 Promotes reviewed chat revisions without mutating output
    sourceinvestigation/         Read-only source verification helpers
    aitest/                      Record/replay chat-completions server for tests

    -- write actions (issues, fix PRs, resolution) --
    actions/                     On-demand single-failure actions behind admin auth
    actiondraft/                 Validates model-generated issue and PR text
    actionverify/                Checks remediation symbols against pinned source
    issues/                      Opens and maintains GitHub issues
    fixpr/                       Drafts minimal code fixes for recurring patterns
    fixruntime/                  Selects the coding-agent runtime for fix PRs
    fixexecutor/                 Clones, runs OpenCode, validates, and returns one staged patch
    chatfix/                     Bridges one chat response into fix generation
    remediationpolicy/           Shared deterministic remediation safety policy
    resolve/                     Admin-marked "resolved" recurring patterns
    patternstate/                Pattern publication + write-side validation
    ghpr/                        Opens PRs that add/update files in one commit
    githubapp/                   Authenticates as a GitHub App installation
    repotemplate/                Fetches a repo's issue/PR markdown templates
    notify/                      SMTP email notifications

    -- serving and deployment --
    server/                      HTTP handler for the Kubernetes-native mode
    auth/                        Admin auth seam (dev / proxy / oauth)
    runtime/                     Swappable agent-execution abstraction
    githubsource/                Small read-only GitHub source reader
    kubernetesdeploy/            Installs a validated consumer bundle with Helm
    onboard/                     `aster onboard`: discovery, presets, doctor, scaffold
    project/                     project.yaml load + validate

    -- support --
    statefile/                   Atomic JSON writes + repo-scoped tracking state
    buildsource/                 Immutable build repository source resolution
    redact/                      Scrubs sensitive values before logging
    credentialenv/               Normalizes secret env vars once at startup
    textutil/                    Small shared string helpers
    e2e/                         Hermetic end-to-end pipeline regression test

  benchmarks/                    Opt-in model-quality benchmarks; gated by RUN_*/BENCH_*
                                 env vars and never run in CI

frontend/                        React 19 + Vite 8 + MUI 9
  public/data/                   Fetcher writes JSON here; Vite serves it
  src/
    hooks/useData.ts             Loads dashboard.json, flakiness.json, jobs/*
    components/ManifestProvider.tsx   Loads manifest.json
configs/example/                 Docs-only minimal project.yaml + full reference
deploy/helm/                     Helm chart for the Kubernetes-native mode
Dockerfile                       Multi-stage image: fetcher + server + SPA
docs/                            onboarding, deployment, configuration, AI,
                                 feature, troubleshooting, and contributor guides
.github/workflows/               ci.yml + reusable-deploy.yml + reusable-clear-cache.yml + image.yml
Makefile                         Local-dev entry points (PROJECT_DIR override)
```

## How the pieces fit

Most changes touch one of five packages. Read this before hunting through the
list above.

```
Prow/TestGrid -> fetcher -> ai -> output -> server -> frontend
                    |        |                 |
                 patterns  skills+tools     actions (admin writes)
```

1. **`fetcher`** discovers jobs and builds, pulls JUnit + artifacts through
   `storage`, and decides what needs analyzing. Start here for anything about
   what data lands in the dashboard.
2. **`ai`** analyzes one failure with the agentic loop: the model calls tools to
   browse artifacts, and floors, critique, and the semantic judge gate the answer
   before it is cached. Start here for analysis quality.
3. **`output`** writes `dashboard.json`, `jobs/*.json`, and the rest of the JSON
   contract. Both deploy paths read the identical contract.
4. **`server`** serves that contract in Kubernetes mode and adds a capability
   descriptor plus admin-gated writes. The Pages path serves the same files
   statically with no server.
5. **`actions`** performs on-demand admin writes (file issue, propose fix, mark
   resolved), reusing the same `issues` and `fixpr` code the scheduled pass uses.

The many small packages are deliberate: several exist to break shared
dependencies between `fetcher`, `actions`, and `server` (for example `resolve`,
`patternstate`, `actiondraft`), which would otherwise import-cycle.

## Setup commands

```bash
# Backend (Go 1.25)
make build           # cd backend && go build -o ../bin/aster ./cmd/aster/
make test            # cd backend && go test ./... -count=1
make tidy            # go mod tidy
make check-repo-map  # AGENTS.md repo layout matches backend/cmd + backend/internal
make check-doc-links # relative links between Markdown files resolve

# Frontend (Node 20+, npm)
make fe-install      # npm ci in frontend/
make fe-check        # tsc -b across the referenced projects
make fe-build        # production build into frontend/dist/

# Kubernetes-native mode (Go 1.25)
make build-server    # cd backend && go build -o ../bin/server ./cmd/server/
make serve           # serve frontend/public/data over HTTP
make dev-actions     # serve SPA + API with admin actions enabled (local auth)
make image           # docker build fetcher + server + SPA into one image
make fixer-image     # drop-in image with git for fix generation
```

## Local development workflow

The fetcher takes `-project-dir=<consumer-repo>`. The default is
`configs/example` (renders an empty dashboard for smoke-testing) so a fresh
engine checkout works without any consumer repo cloned.

```bash
# Frontend dev loop with real production data (no AI calls)
make fetch-data-quick PROJECT_DIR=../<your-consumer-repo>
make dev                       # http://localhost:5173 with HMR

# With AI analysis (needs creds)
export AI_TOKEN=<token>
export AI_ENDPOINT=<chat-completions-url>
export AI_MODEL=<model-id>
make fetch-data-ai-quick PROJECT_DIR=../<your-consumer-repo>
make dev

# Frontend-only iteration (no Go, no GCS): drop pre-built JSON from a
# deployed site's gh-pages publish into frontend/public/data/, then `make dev`.

# Preview the server-mode UI *with* admin actions (File issue / Propose fix /
# Mark resolved). `make dev` is read-only: it has no capability endpoint, so the
# action buttons never render. dev-actions serves the built SPA from the API
# server with AUTH_MODE=dev (every request authenticated as an admin; local
# only), so the buttons act. Set BOT_TOKEN to a real token for writes to reach
# GitHub. It serves a static build (no HMR); rerun to pick up frontend changes.
make dev-actions PROJECT_DIR=../<your-consumer-repo>   # http://localhost:8080
```

Vite serves `frontend/public/` at the site root, so any JSON the fetcher
writes there is immediately visible to the dev server. No `VITE_BASE_PATH`
needed for local dev (defaults to `/`).

## Testing instructions

- **All tests:** `cd backend && go test ./... -count=1` (also `make test`)
- **Single package:** `cd backend && go test ./internal/ai/... -count=1`
- **Single test:** `go test ./internal/ai -run TestService_CacheKeyShape -v`
- **Race detector** (AI subsystem only): `go test -race -count=1 ./internal/ai/...`
- **Vet:** `cd backend && go vet ./...`
- **Format:** `cd backend && gofmt -l .` (then `gofmt -w .` to fix)
- **Static analysis:** `cd backend && staticcheck ./...` (expected clean; any
  warning from code you touched is a regression).

CI (`.github/workflows/ci.yml`) runs build + test + vet on backend and
build + lint on frontend. CI does not run staticcheck; please still run it
locally before opening a PR.

### Anchor pin tests

Prompt text in `agentic.go`, `responseformat.go`, and `critique.go` is pinned
by tests (e.g. `TestResponseFormatFooter_L4Step1Anchors`,
`TestResponseFormatFooter_L4Step3DepthAnchors`,
`TestAgToolDocs_NoToolSpecificLeaks`). Edit the prompt text? Update the
anchor test in the same commit, or the test will fail loudly. These exist
to prevent unintended drift in carefully tuned prompts.

### Cache invariance

The AI cache is on-disk JSON keyed by mode + hash. Changing the agentic
cache schema (`agenticCacheData` in `agentic.go`) requires bumping
`currentCritiqueVersion` if the change makes existing entries semantically
wrong; otherwise leave it alone so warm caches survive engine upgrades. See
`reanalysisRequired` in `service.go` for the full revalidation gate.

## Code style and conventions

- **Idiomatic Go.** Match the surrounding file's style. Prefer short
  factual comments that describe current behavior. Do not narrate session
  history or iteration ("L.4 Step 2", "rubber-duck #3", "after the bug
  with X", etc.) - those got stripped repo-wide; do not reintroduce them.
- **Comment length:** terse. Most exported types/functions get 1-3 short
  lines; complex algorithms get more, but stay focused on what + why,
  not how the code came to be written that way.
- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. Surface enough
  context for the operator to find the failing artifact / cache key.
- **Logging:** `log.Printf` with a leading emoji/icon and the test or job
  identifier. See `service.go` for the canonical patterns
  (`🔍 Analyzing:`, `⏭ Skipping transient:`).
- **`docs/` is for users, not design history.** Document current behavior and
  how to use it. Do not add decision records, rationale for alternatives that
  were considered and rejected, or migration narratives - those belong in the
  issue or pull request. A change that only records a decision needs no docs
  update at all.
- **No new linting/build/test tools** without a strong reason. CI is
  intentionally minimal; staticcheck is run locally.

## AI subsystem orientation

If you're touching anything under `backend/internal/ai/`, read
[`docs/agentic.md`](docs/agentic.md) and [`docs/skills.md`](docs/skills.md)
first. Quick map:

- **Single path:** every failure is analyzed by the agentic loop (cache
  `mode: "agentic"`). There is no mode selection and no tools-free fallback;
  an endpoint without function-calling marks failures unavailable.
- **Agentic loop:** `agentic.go` runs up to `MaxIters` rounds (default 15).
  Each round: send conversation → model returns tool calls → engine runs
  tools → results appended → repeat until model returns a final
  `analysisResponse` JSON or budget exhausted.
- **Quality floors:** `min_tool_calls`, `min_gcs_bytes`, model byte budget,
  critique pass, and critique version are enforced by the centralized cache
  policy. A cache hit that fails a current floor is re-analyzed.
- **Critique gate** (`critique.go`): deterministic regex judge runs after
  every draft. Catches investigation-as-remediation, hallucinated artifact
  paths, fabricated import paths, etc. Re-prompts the model with feedback;
  caches the result with the critique version it passed under. Skill recipes
  extend this gate when present.
- **Skills** (`skills/`): consumer-owned recipe registry. Each recipe pairs
  a failure signal with required evidence the model must read before
  claiming that class of failure. The loaded skill hash is retained as
  provenance and changes affect new analyses only.
- **Tools** (`tools/`): `filesystem` (list/read/tail/grep over GCS),
  optional `k8s` discovery helpers, and read-only `repotree` tools when a build
  resolves to a pinned source revision. No shell or write tools, ever. No
  browser tools.
- **Prompt composition** (`compose.go`): engine `BasePrompt` + consumer's
  `prompts/system.md` + engine `ResponseFormatFooter`. The fetcher hard-
  errors at startup if `-ai` is enabled and `prompts/system.md` is missing
  or whitespace-only.

## Project configuration ownership

Engine ships the AI defaults; consumer overrides per project. The contract:

- **Engine-owned:** BasePrompt, ResponseFormatFooter, critique contract,
  tool schemas, cache shape.
- **Consumer-owned** (in `project.yaml`): bucket, dashboard, branding,
  email notification routing, and the inlined `ai.*` agentic tuning (floors
  `min_tool_calls` / `min_gcs_bytes`, `max_iters`, `timeout`, `tools`,
  `critique.max_retries`). SMTP passwords stay in deployment secrets.
- **Consumer-owned** (in `prompts/system.md`): project-specific AI
  knowledge. Mandatory; injected verbatim between BasePrompt and
  ResponseFormatFooter.

Never check engine-side per-project config into this repo. The
`configs/example/` directory is documentation-only and not loaded by any
live deploy.

## Commit conventions

- **No backward compatibility before v1.** Until the first v1 release,
  breaking changes are allowed anywhere: Go APIs, `project.yaml` fields,
  the JSON output contract, cache schemas, server endpoints, chart values,
  and CLI flags. Do not add aliases, shims, deprecation windows, dual-read
  fallbacks, or migration code for old state unless a maintainer explicitly
  asks. Change the callers, delete the old path, and note the break in the
  pull request description. When in doubt, grep for callers: if nothing in
  any consumer's `project.yaml` or the engine code paths references a given
  branch, it is a deletion candidate.
- **Conventional, terse commit messages.** Use a single-line subject. Put the
  detailed rationale and verification in the pull request description.
- **Every pull request needs a `release-note` block.** CI
  (`.github/workflows/release-note.yml`) fails without one, and the block
  supplies that change's release-note text. `.github/PULL_REQUEST_TEMPLATE.md`
  carries it and the rest of the expected sections, and is the canonical author
  guidance; `hack/check-release-note.py` is what actually enforces it.

  The description must hold **exactly one** backtick-fenced block whose info
  string is `release-note`. The four-backtick wrapper below keeps its inner
  example from being counted.

  ````
  ```release-note
  What changed, and what it means for someone upgrading Aster.
  ```
  ````

  Write it for someone upgrading Aster, not for a reviewer: the symptom, then
  the cause, then the fix, as finished prose rather than a summary of the diff.
  Call out breaking changes explicitly. Use `NONE` when nothing a user would
  notice changed: tests, refactors, repo tooling, or documentation corrections.
  The checker rejects missing, duplicate, empty, or unterminated blocks and
  unclosed HTML comments.

## Common pitfalls

- **Fetcher silently fails with empty output.** Almost always means
  `-project-dir` defaults to `.` and there's no `project.yaml` there.
  After the consumer split, always pass `PROJECT_DIR=...` when running
  locally.
- **AI cache "thrashing" on every run.** Means a floor or schema change
  invalidated all entries. Check `reanalysisRequired` and the
  `agenticCacheKey` shape. Cache-key shape changes are catastrophic; tread
  carefully. A `preliminary` analysis is retried on later passes, but only
  until `maxPreliminaryAttempts` is spent; see `agentic.go`.
- **Anchor pin test failures.** You edited prompt text without updating
  the anchor test. Update both in the same commit.
- **Stale-mode cache entries.** A cached analysis whose `mode` is not
  `"agentic"` (e.g. from an earlier pipeline) is treated as a miss and
  re-analyzed on the next fetcher run. No action needed; it self-heals.
- **In-cluster chat-completions endpoints** are unreachable from GitHub-hosted runners.
  Use the Kubernetes-native path (fetch runs in-cluster next to the endpoint) OR,
  on the Pages path, set `skip-fetch: true` and commit pre-fetched `data/` to the
  consumer repo.
- **Per-deploy `builds:` input.** Trade-off between history depth and
  cron-window budget. Halving this halves cold-cache fetch time;
  doubling it deepens history but risks overrunning the cron interval.

## Pointers to deeper docs

- `docs/onboarding-a-new-project.md` - common onboarding and scaffold flow.
- `docs/project-configuration.md` - strict project.yaml field reference.
- `docs/github-pages.md` - GitHub Actions and Pages deployment.
- `docs/troubleshooting.md` - first-deploy failures and checks.
- `docs/architecture/in-process-analyzer.md` - authoritative analyzer architecture and contributor map.
- `docs/agentic.md` - authoritative analysis loop, quality gates, cache, and operations.
- `docs/skills.md` - consumer-side recipe registry format + hashing.
- `docs/writing-prompts.md` - how `prompts/system.md` slots into the
  composed prompt and what makes a good project addendum.
- `docs/ai-providers.md` - endpoint shape requirements (OpenAI-style chat
  completions + function calling) and per-provider notes
  (Copilot, OpenAI, Nvidia Dynamo/NIM, vLLM, Ollama, ...).
- `docs/server.md` - server mode endpoints and the capability seam.
- `docs/kubernetes.md` - first-run Kubernetes deployment quickstart.
- `docs/kubernetes-platform.md` - platform ownership, prerequisites, and lifecycle.
- `docs/kubernetes-reference.md` - architecture, modes, chart values, upgrades,
  and advanced Kubernetes operations.

## When in doubt

The repo is pre-v1 and under heavy development with only two internal
consumers. Breaking changes need no backward compatibility, and we prefer
deleting dead code over carrying compat branches. If you're unsure whether
some scaffolding is still load-bearing, grep for callers - if nothing
references it in either consumer's `project.yaml` or the engine code paths,
it's a deletion candidate.
