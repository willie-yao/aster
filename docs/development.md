# Local development

Build, test, and run the engine against a consumer repo locally. For the full
command catalog and testing matrix, see [AGENTS.md](../AGENTS.md).

## Build and test

```bash
make build && make test              # backend (Go 1.25)
make fe-install && make fe-check     # frontend (Node 20+)
```

## Run the fetcher locally

The fetcher takes `-project-dir=<consumer-repo>`, a directory holding
`project.yaml` and `prompts/system.md`. It writes JSON into
`frontend/public/data/`, which the Vite dev server serves at the site root, so
output is immediately visible.

```bash
make fetch-data PROJECT_DIR=../your-consumer-repo
make dev                             # http://localhost:5173 with HMR
```

Add `-ai` and set `AI_TOKEN`, `AI_ENDPOINT`, and `AI_MODEL` to populate AI
summaries. See [ai-providers.md](ai-providers.md) for endpoint details.

For a one-off run without the Makefile:

```bash
./bin/fetcher -project-dir=../your-consumer-repo -out=frontend/public/data \
  -builds=3 -workers=5
```

## Frontend-only iteration

To work on the UI without running the fetcher, drop pre-built JSON from a
deployed site into `frontend/public/data/`, then `make dev`.

## Previewing admin actions locally

`make dev` (Vite) is read-only: it has no `/api/capabilities` endpoint, so the
admin action buttons (File issue, Propose fix, Mark resolved) never render. To
preview them, serve the built SPA from the API server with actions enabled:

```bash
make dev-actions PROJECT_DIR=../your-consumer-repo   # http://localhost:8080
```

This builds the frontend and runs the server in `proxy` auth mode with no
trusted header, which auto-authorizes every request as an admin, so no OAuth
setup is needed. `PROJECT_DIR` resolves the issue/fix target repos. It serves a
static build (no HMR), so rerun it to pick up frontend changes.
