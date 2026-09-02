# Local development

This guide is for contributors working on the engine. See [`CONTRIBUTING.md`](../CONTRIBUTING.md) for the contribution workflow and [Testing](testing.md) for the full validation matrix.

## Prerequisites

- Go 1.25 as declared by `backend/go.mod`
- Node.js 20 or newer
- npm
- `staticcheck` for full backend validation

Docker, Helm, and kubectl are needed only for container or Kubernetes work.

## Build and test

```bash
make build
make test
make fe-install
make fe-check
```

`make fe-check` uses `tsc -b`. The root TypeScript config is a solution file, so plain `tsc --noEmit` would not walk the referenced projects.

## Run the fetcher locally

The fetcher takes `-project-dir=<consumer-repo>`, a directory holding `project.yaml` and, when AI is enabled, `prompts/system.md`. It writes JSON into `frontend/public/data`, which Vite serves immediately.

```bash
make fetch-data-quick PROJECT_DIR=../my-consumer
make dev
```

The Vite server runs at <http://localhost:5173> with HMR.

For AI analysis:

```bash
export AI_TOKEN=<token>
export AI_API=chat_completions AI_ENDPOINT=<provider-api-url>
export AI_MODEL=<model-id>
# Optional: export AI_REASONING_EFFORT=high
make fetch-data-ai-quick PROJECT_DIR=../my-consumer
make dev
```

For a one-off run, build the binary first:

```bash
make build
./bin/aster -project-dir=../my-consumer -out=frontend/public/data \
  -builds=3 -workers=5
```

## Frontend-only iteration

Copy the public JSON files from a deployed dashboard into `frontend/public/data`, then run `make dev`. Do not copy operational files such as `ai_cache.json`, `ai_traces.json`, or issue and fix state.

`make snapshot-data` does the copying for you over HTTP:

```bash
make snapshot-data SITE=https://your-dashboard
make dev
```

It mirrors only what a deployed site publishes at `/data`, which is the same contract the SPA reads. Operational files are not published there and are not mirrored. Unlike a local fetch, the snapshot carries published AI analyses, recurring patterns, and pull request triage, so the pages that render analysis have real content without any AI credentials.

## Develop against the authenticated UI

`make dev` alone serves no `/api`, so the sign-in control, action buttons, analysis chat, and the operator pages never render. `make dev-mock` keeps Vite and HMR while a local API server answers `/api` from in-memory fakes:

```bash
make snapshot-data SITE=https://your-dashboard   # once, for the dashboard content
make dev-mock
```

Open <http://localhost:5173> and choose **Sign in**. There is no GitHub round trip: the session is established immediately, and **Sign out** returns you to the anonymous state, so both are reachable while iterating.

Every feature the deployed dashboard advertises is enabled, and the capability descriptor, routes, auth middleware, CSRF guard, and JSON shapes are the production ones. Only the work behind them is fabricated:

- Issue and fix drafting, both the immediate preview and the asynchronous request with its pending, ready, confirmed, and cancelled states. Drafts quote the published analysis and confirmation returns an `example.invalid` link instead of writing to GitHub.
- Analysis chat, including the streamed turn phases, citations, proposed revisions, and the chat-to-fix preview.
- On-demand pull request and shared failure escalation.
- Analysis health, AI usage, fetch status, and pattern diagnostics, backed by fabricated files written into the data directory on first start. Delete them with `make clean-mock` to regenerate.

Resolution is not faked. **Mark resolved** runs the real code against the snapshot, so it computes a real watermark, writes `frontend/public/data/resolved.json`, and reports the same refusals production does when a pattern is recovered or its evidence is unavailable.

Useful knobs:

- `MOCK_LATENCY` sets how long a fabricated model call takes, so pending and streaming states can be slowed down or removed (`MOCK_LATENCY=20s`, `0s`).
- `MOCK_LOGIN` sets the admin login a sign-in establishes.
- `MOCK_HOST` and `MOCK_PORT` move the API server, which binds `127.0.0.1:8080` by default. The Vite proxy follows both.
- `make mock-server` runs the API alone, for curl or for pointing an already-running frontend at it.

The mock server grants admin to anyone who reaches it and carries no credentials. It binds loopback by default; keep it that way.

## Preview admin actions against real services

The Vite server has no `/api/capabilities`, so action buttons do not render. Serve the built SPA through the API server instead:

```bash
make dev-actions PROJECT_DIR=../my-consumer
```

This runs at <http://localhost:8080> with local development authentication. It serves a static build and has no HMR.

File issue and Mark resolved work with a real `BOT_TOKEN`. Propose fix requires an enabled Agent Sandbox Fix runtime and is not exercised by the local `dev-actions` server alone.

## Kubernetes-native development

```bash
make build-server
make build-worker
make image
helm lint deploy/helm/aster \
  --set-file project.config=configs/example/project.yaml \
  --set-file project.systemPrompt=configs/example/prompts/system.md
make helm-check
```

Use [Kubernetes operator reference](kubernetes-reference.md) for runtime configuration.
