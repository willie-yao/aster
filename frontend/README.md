# Frontend

The dashboard frontend is a React 19, TypeScript, Vite 8, and MUI application.
It renders the JSON contract produced by the Go fetcher.

## Data contract

Vite serves `public/data` at `<base>/data`. The main files are:

- `manifest.json`: branding and public project configuration
- `dashboard.json`: job summaries
- `jobs/*.json`: build and test details
- `flakiness.json`: cross-run failure history
- `search-index.json`: client-side search data
- `resolved.json`: maintainer resolution state when present

`ManifestProvider` loads project branding once. The data hooks in
`src/hooks/useData.ts` load the remaining files.

## Static and server modes

The same build supports both deployment modes:

- GitHub Pages has no API server and remains read-only.
- Kubernetes-native mode serves `/api/capabilities`. The frontend enables only
  the interactive features advertised by that descriptor.

A failed capability probe is expected on Pages and leaves the static defaults in
place.

## Development

From the repository root:

```bash
make fetch-data-quick PROJECT_DIR=../my-consumer
make dev
```

The Vite server runs at <http://localhost:5173> with HMR.

For frontend-only work, copy a deployed site's JSON into `public/data` and run
`make dev`.

## Validation

```bash
make fe-check
make fe-lint
make fe-build
```

`make fe-check` uses TypeScript build mode because the root `tsconfig.json` is a
solution file with project references.

## Interactive actions

The Vite server has no `/api/capabilities`, so action buttons do not render. Use:

```bash
make dev-actions PROJECT_DIR=../my-consumer
```

This builds the SPA and serves it from the Go API server at
<http://localhost:8080>. It uses local development authentication and has no HMR.
