# AGENTS.md

Guidance for AI coding agents working on `prow-ai-dashboard`. See
[`README.md`](README.md) for the human-facing introduction and
[`docs/`](docs/) for deep dives.

## Project overview

`prow-ai-dashboard` is the **reusable engine** for AI-powered Prow/TestGrid
dashboards. It is consumed by lightweight per-project repos (the
"consumers") via a reusable GitHub Actions workflow. The engine repo holds
all the code; consumer repos hold only `project.yaml` + `prompts/system.md`
+ a ~20-line workflow file. See [`README.md`](README.md) for the current
list of live consumers.

The data flow per scheduled deploy is:

```
Consumer workflow (cron)
   └─> Reusable workflow `.github/workflows/reusable-deploy.yml`
         ├─> Checks out engine + consumer side-by-side
         ├─> Builds backend/cmd/fetcher
         ├─> fetcher -project-dir=<consumer> -out=engine/frontend/public/data
         │     ├─> Loads consumer's project.yaml + prompts/system.md
         │     ├─> Discovers prow jobs from kubernetes/test-infra
         │     ├─> Fetches recent builds + JUnit XML from GCS
         │     ├─> Runs AI analysis per failing test (agentic, cached)
         │     └─> Writes manifest.json + dashboard.json + jobs/*.json + ...
         ├─> Builds frontend/ (Vite, with VITE_BASE_PATH for gh-pages subpath)
         └─> Deploys built site via actions/deploy-pages to consumer's GH Pages
```

## Repo layout

```
backend/                       Go 1.25
  cmd/
    fetcher/                   Main entrypoint; one binary per deploy
    server/                    Kubernetes-native API server (read parity + capabilities)
    worker/                    Continuous watch worker (in-cluster incremental fetch)
    ai-toolcall-spike/         Throwaway probe; safe to ignore
    _manifest_check/           Build-time check on manifest schema
  internal/
    ai/                        AI orchestration (most active area)
      ai.go                    Chat client, JSON parsing, header handling
      agentic.go               Agentic loop + cache shape + floor gates
      service.go               Cache-key, staleness, shouldReanalyze
      critique.go              Deterministic regex judge that gates drafts
      skills/                  Consumer-owned recipe registry
      tools/                   Function-calling tools (filesystem, k8s)
      modules/
        universal/             Universal agentic module (builds the seed prompt)
      baseprompt.go            BasePrompt (engine-owned)
      responseformat.go        ResponseFormatFooter (engine-owned)
      compose.go               Composes: BasePrompt + consumer system.md + footer
      cache.go                 On-disk cache (JSON, keyed by mode+hash)
    aggregator/                Roll-ups across builds
    artifacts/                 Build-log + artifact parsing
    collectors/                Pluggable; `generic` ships in-tree
    fetcher/                   AIModuleRegistry, CollectorRegistry wiring
    storage/                  Pluggable artifact store (gcs / gcsweb backends)
    prowbuild/                Prow path layout, build info, JUnit + job discovery
    junit/                     JUnit XML parser
    models/                    Wire-format types (BuildResult, AIAnalysis, ...)
    notify/                    Slack notifications
    output/                    JSON writers (dashboard.json, jobs/, ...)
    project/                   project.yaml load + validate
    prow/jobconfig/            kubernetes/test-infra job parsing
    server/                    HTTP handler: /data/* read parity + /api/capabilities

frontend/                      React 19 + Vite 8 + Tailwind 4
  public/data/                 Fetcher writes JSON here; Vite serves it
  src/
    hooks/useData.ts           Loads dashboard.json, flakiness.json, jobs/*
    components/ManifestProvider.tsx   Loads manifest.json
configs/example/               Docs-only sample project.yaml + prompts/
deploy/helm/                   Helm chart for the Kubernetes-native mode
Dockerfile                     Multi-stage image: fetcher + server + SPA
docs/                          agentic.md, ai-providers.md, skills.md,
                               writing-prompts.md, onboarding-a-new-project.md
.github/workflows/             ci.yml + reusable-deploy.yml + reusable-clear-cache.yml + image.yml
Makefile                       Local-dev entry points (PROJECT_DIR override)
```

## Setup commands

```bash
# Backend (Go 1.25)
make build           # cd backend && go build -o ../bin/fetcher ./cmd/fetcher/
make test            # cd backend && go test ./... -count=1
make tidy            # go mod tidy

# Frontend (Node 20+, npm)
make fe-install      # npm ci in frontend/
make fe-check        # tsc --noEmit
make fe-build        # production build into frontend/dist/

# Kubernetes-native mode (Go 1.25)
make build-server    # cd backend && go build -o ../bin/server ./cmd/server/
make serve           # serve frontend/public/data over HTTP
make image           # docker build fetcher + server + SPA into one image
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
- **Static analysis:** `cd backend && staticcheck ./...`
  - One unrelated pre-existing warning is expected in
    `cmd/ai-toolcall-spike/main.go`. Any new warning from code you touched is a
    regression.

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
`belowCurrentAgenticFloor` in `service.go` for the full revalidation gate.

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
  critique pass, skill-set hash - all enforced in
  `belowCurrentAgenticFloor`. A cache hit that fails any current floor is
  re-analyzed.
- **Critique gate** (`critique.go`): deterministic regex judge runs after
  every draft. Catches investigation-as-remediation, hallucinated artifact
  paths, fabricated import paths, etc. Re-prompts the model with feedback;
  caches the result with the critique version it passed under. Auto-enabled
  when skill recipes are present.
- **Skills** (`skills/`): consumer-owned recipe registry. Each recipe pairs
  a failure signal with required evidence the model must read before
  claiming that class of failure. Hash of loaded skills participates in
  cache invalidation.
- **Tools** (`tools/`): `filesystem` (list/read/tail/grep over GCS) +
  `k8s` (discover_clusters, discover_controllers, ...). Read-only. No
  shell or write tools, ever. No browser tools. (See "What we explicitly
  are NOT doing" in any historical plan.)
- **Prompt composition** (`compose.go`): engine `BasePrompt` + consumer's
  `prompts/system.md` + engine `ResponseFormatFooter`. The fetcher hard-
  errors at startup if `-ai` is enabled and `prompts/system.md` is missing
  or whitespace-only.

## Project configuration ownership

Engine ships the AI defaults; consumer overrides per project. The contract:

- **Engine-owned:** BasePrompt, ResponseFormatFooter, critique contract,
  tool schemas, cache shape.
- **Consumer-owned** (in `project.yaml`): bucket, dashboard, branding,
  the inlined `ai.*` agentic tuning (floors `min_tool_calls` /
  `min_gcs_bytes`, `max_iters`, `timeout`, `critique`, `tools`,
  `evidence_injection`), evidence selection (`ai.evidence.machine_logs`,
  `ai.evidence.controller_logs`, `ai.evidence.build_log_patterns`).
- **Consumer-owned** (in `prompts/system.md`): project-specific AI
  knowledge. Mandatory; injected verbatim between BasePrompt and
  ResponseFormatFooter.

Never check engine-side per-project config into this repo. The
`configs/example/` directory is documentation-only and not loaded by any
live deploy.

## Commit conventions

- **No backward-compat scaffolding by default.** While the engine is
  under heavy development with a small known set of consumers, the
  project prefers deleting dead code over maintaining compat branches.
  When in doubt, grep for callers: if nothing in any consumer's
  `project.yaml` or the engine code paths references a given branch, it
  is a deletion candidate.
- **Conventional, terse commit messages.** Opening paragraph explains
  rationale; bulleted list describes the changes; closing line confirms
  `go build / vet / test / staticcheck` are clean.

## Common pitfalls

- **Fetcher silently fails with empty output.** Almost always means
  `-project-dir` defaults to `.` and there's no `project.yaml` there.
  After the consumer split, always pass `PROJECT_DIR=...` when running
  locally.
- **AI cache "thrashing" on every run.** Means a floor or schema change
  invalidated all entries. Check `belowCurrentAgenticFloor` and the
  `agenticCacheKey` shape. Cache-key shape changes are catastrophic; tread
  carefully.
- **Anchor pin test failures.** You edited prompt text without updating
  the anchor test. Update both in the same commit.
- **Stale-mode cache entries.** A cached analysis whose `mode` is not
  `"agentic"` (e.g. from an earlier pipeline) is treated as a miss and
  re-analyzed on the next fetcher run. No action needed; it self-heals.
- **In-cluster chat-completions endpoints** are unreachable from GitHub-hosted runners.
  Use `runs-on:` + a self-hosted runner OR set `skip-fetch: true` and
  commit pre-fetched `data/` to the consumer repo.
- **Per-deploy `builds:` input.** Trade-off between history depth and
  cron-window budget. Halving this halves cold-cache fetch time;
  doubling it deepens history but risks overrunning the cron interval.

## Pointers to deeper docs

- `docs/onboarding-a-new-project.md` - full worked example for adding a new
  consumer repo.
- `docs/agentic.md` - agentic mode, tool docs, floors, critique gate.
- `docs/skills.md` - consumer-side recipe registry format + hashing.
- `docs/writing-prompts.md` - how `prompts/system.md` slots into the
  composed prompt and what makes a good project addendum.
- `docs/ai-providers.md` - endpoint shape requirements (OpenAI-style chat
  completions + function calling) and per-provider notes
  (Copilot, OpenAI, Nvidia Dynamo/NIM, vLLM, Ollama, ...).
- `docs/server.md` - server mode endpoints and the capability seam.
- `docs/kubernetes.md` - Kubernetes-native deploy: fetcher CronJob + server
  from a shared volume, via the Helm chart in `deploy/helm`.

## When in doubt

The repo is under heavy development with only two internal consumers; we
prefer deleting dead code over carrying compat branches. If you're unsure
whether some scaffolding is still load-bearing, grep for callers - if
nothing references it in either consumer's `project.yaml` or the engine
code paths, it's a deletion candidate.
