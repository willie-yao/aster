# Testing

The engine has deterministic backend, frontend, and end-to-end tests. Live model quality evaluations are maintained separately from the engine.

## Full validation

Run these before opening a pull request that affects both backend and frontend:

```bash
make build
make fmt-check lint
go -C backend mod tidy -diff
go -C backend vet ./...
go -C backend test ./... -race -count=1
cd frontend && npm ci && npx tsc -b && npm run lint && npm run build
```

CI runs build, race-enabled tests, vet, lint, module-tidiness, and formatting checks for the main backend module plus frontend type check, tests, lint, and root/subpath builds. The backend job uses the pinned golangci-lint suite, including staticcheck; `make lint` installs and runs the same suite locally.

Check Go formatting with:

```bash
make fmt-check
```

## Focused backend tests

```bash
# One package tree.
cd backend && go test ./internal/ai/... -count=1

# One test.
cd backend && go test ./internal/ai -run TestService_CacheKeyShape -v

# AI subsystem with the race detector.
cd backend && go test -race -count=1 ./internal/ai/...

# Headless onboarding and concrete terminal adapters.
cd backend && go test ./internal/onboard/... ./internal/kubernetesdeploy ./cmd/aster -count=1
```

The `onboard` package owns planning, application, and the guided workflow through
an injected prompter. `onboard/terminal` owns process streams, TTY detection, and
concrete input implementations. Headless onboarding and Kubernetes deployment
must not depend on the terminal adapter or its UI libraries.

Prompt text in `agentic.go`, `responseformat.go`, and `critique.go` is pinned by anchor tests. Update the relevant anchor test in the same change as intentional prompt edits.

## Test helpers

Keep Go helpers package-local in `_test.go` files. Share arrangement and checked
serialization, while leaving expected outcomes, intentionally invalid inputs,
file modes, clocks, and service inputs visible in each case. Use fresh
fixtures for table rows rather than mutating the previous row's state.

Frontend SSR tests use `frontend/tests/helpers/ssr.ts`. Load each suite's
components, application contexts, and theme in one `withSSR` callback so they
share a Vite module graph. The helper closes that suite's server on success or
failure. Keep differing render providers and viewport configuration local.

`failedJUnitRun` supplies the shared grounded JUnit fixture with fresh nested
objects. `MemoryStorage` supplies only the three storage methods used by tests.
Keep bespoke fixtures when missing evidence, source identity, or malformed
storage is the subject, and do not share a live Vite server or mutable fixtures
across suites.

## End-to-end pipeline tests

`internal/e2e` runs `fetcher.Run` through discovery, artifact parsing, aggregation, scripted AI analysis, and output writing against local fixtures. It has no network, model, or GCS dependency.

```bash
make e2e
```

The harness uses:

- The local storage provider with a fixture tree that mirrors Prow storage.
- `internal/aitest.ScriptServer` for ordered deterministic model responses.
- `internal/aitest.ReplayServer` for recorded request and response fixtures.

`make e2e` also runs the hermetic email and fix-PR loop in `internal/fetcher`. That scenario uses temporary Prow artifacts, a fake GitHub transport, a deterministic fix agent, and an in-memory email sender. It covers the recurring-pattern alert, action links, fix tracking, and deduplication across repeated passes. A second bridge test proves the finalized pattern bridge reaches the same email side effects. Neither test sends real email, calls GitHub, or runs OpenCode.

Fixtures live under `backend/internal/e2e/testdata`. Scrub secrets and private artifact content before committing a recording. The email-loop test writes its compact sequential artifacts into temporary directories instead of committing additional fixture trees.

## AI quality evaluations

Personal model-quality evaluations live in the separate `aster-benchmarks` repository. They are not required to build, test, or deploy Aster. Use that repository's runner and documentation for pinned-engine, provider-free checks and explicitly authorized live evaluations.

Product unit tests, hermetic E2E tests, and the small cache and flakiness performance benchmarks remain in this repository.

## Documentation validation

`make check-doc-links` validates Git-tracked Markdown files. For generated files,
use `python3 hack/check-doc-links.py --root <root> <file>...`, with paths relative
to that root. Explicit selection needs no Git repository and rejects source
symlinks and paths escaping the root. Cleanroom uses this checker for both engine
documentation and the generated consumer deployment README.

When editing Markdown:

- Verify local links and heading anchors.
- Validate generated scaffold text with `go test ./internal/onboard`.
- Run `make helm-check` when Helm templates, packaged files, examples, or values change. It lints the chart, verifies the default in-process render, Agent Sandbox runtime values, invalid-value failures, and the operational helpers.
