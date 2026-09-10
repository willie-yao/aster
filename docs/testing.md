# Testing

The engine has deterministic backend, frontend, and end-to-end tests. Live model quality evaluation is opt-in and is not a normal CI gate.

## Full validation

Run these before opening a pull request that affects both backend and frontend:

```bash
make build
cd backend && go vet ./... && go test ./... -count=1 && staticcheck ./...
cd ../frontend && npm ci && npx tsc -b && npm run lint && npm run build
```

CI runs build, test, and vet for the main backend module plus frontend type check, tests, lint, and root/subpath builds. A benchmark-scoped job runs provider-free harness tests, tidy, and vet when backend or benchmark changes select it. Live benchmark gates are explicitly disabled. CI does not run `staticcheck`, so run it locally for backend changes.

Check Go formatting with:

```bash
cd backend
gofmt -l .
```

## Focused backend tests

```bash
# One package tree.
cd backend && go test ./internal/ai/... -count=1

# One test.
cd backend && go test ./internal/ai -run TestService_CacheKeyShape -v

# AI subsystem with the race detector.
cd backend && go test -race -count=1 ./internal/ai/...
```

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

Fixtures live under `backend/internal/e2e/testdata`. Benchmark fixtures live separately under `backend/benchmarks/testdata`. Scrub secrets and private artifact content before committing a recording. The email-loop test writes its compact sequential artifacts into temporary directories instead of committing additional fixture trees.

## AI quality benchmark

The opt-in benchmarks live in a separate Go module at `backend/benchmarks`. The main module's `go build ./...`, `go test ./...`, and `go vet ./...` do not compile it. CI runs its provider-free harness tests and checks module metadata and vet when backend or benchmark changes select it. Live cases remain gated behind their own `RUN_*` or `BENCH_*` environment variable. Provider-free harness tests can be run directly with `go -C backend/benchmarks test ./... -count=1` with the live benchmark gates unset.

Use a Go toolchain at least as new as `backend/benchmarks/go.mod` requires on
`PATH`, including for the fixture verifier's nested `go test` commands.

```bash
RUN_AI_BENCHMARK=1 \
AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
AI_MODEL=<model-id> AI_TOKEN=<token-or-placeholder> \
  go -C backend/benchmarks test . -run TestAIBenchmark -v -timeout 60m
```

Set `BENCH_PROJECT_DIR` to a consumer repository to load its prompt and AI settings. The benchmark also accepts `BENCH_MAX_ITERS`, `BENCH_TIMEOUT`, `BENCH_MIN_TOOL_CALLS`, `BENCH_MIN_GCS_BYTES`, and `BENCH_CRITIQUE_RETRIES` overrides.

There is no checked-in A/B comparison command. Compare benchmark logs or saved results when evaluating two models or configurations.

The benchmark reports the unique successful filesystem and Kubernetes Tool names and per-Tool call counts for each trial.

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
