# Local development

This guide is for contributors working on the engine. See
[`CONTRIBUTING.md`](../CONTRIBUTING.md) for the contribution workflow and
[Testing](testing.md) for the full validation matrix.

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

`make fe-check` uses `tsc -b`. The root TypeScript config is a solution file, so
plain `tsc --noEmit` would not walk the referenced projects.

## Run the fetcher locally

The fetcher takes `-project-dir=<consumer-repo>`, a directory holding
`project.yaml` and, when AI is enabled, `prompts/system.md`. It writes JSON into
`frontend/public/data`, which Vite serves immediately.

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
make fetch-data-ai-quick PROJECT_DIR=../my-consumer
make dev
```

For a one-off run, build the binary first:

```bash
make build
./bin/fetcher -project-dir=../my-consumer -out=frontend/public/data \
  -builds=3 -workers=5
```

## Frontend-only iteration

Copy the public JSON files from a deployed dashboard into
`frontend/public/data`, then run `make dev`. Do not copy operational files such
as `ai_cache.json`, `ai_traces.json`, or issue and fix state.

## Preview admin actions

The Vite server has no `/api/capabilities`, so action buttons do not render.
Serve the built SPA through the API server instead:

```bash
make dev-actions PROJECT_DIR=../my-consumer
```

This runs at <http://localhost:8080> with local development authentication. It
serves a static build and has no HMR.

File issue and Mark resolved work with a real `BOT_TOKEN`. Propose fix also needs
`opencode` and git installed on the host.

## Kubernetes-native development

```bash
make build-server
make build-worker
make image
helm lint deploy/helm/prow-ai-dashboard \
  --set-file project.config=configs/example/project.yaml \
  --set-file project.systemPrompt=configs/example/prompts/system.md
make helm-check
```

Use [Kubernetes-native deployment](kubernetes.md) for runtime configuration.
