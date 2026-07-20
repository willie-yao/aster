# Testing

The engine has deterministic backend, frontend, and end-to-end tests. Live model
quality evaluation is opt-in and is not a normal CI gate.

## Full validation

Run these before opening a pull request that affects both backend and frontend:

```bash
make build
cd backend && go vet ./... && go test ./... -count=1 && staticcheck ./...
cd ../frontend && npm ci && npx tsc -b && npm run lint && npm run build
```

CI runs backend build, test, and vet plus frontend type check, lint, and build.
CI does not run `staticcheck`, so run it locally for backend changes.

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

Prompt text in `agentic.go`, `responseformat.go`, and `critique.go` is pinned by
anchor tests. Update the relevant anchor test in the same change as intentional
prompt edits.

## End-to-end pipeline tests

`internal/e2e` runs `fetcher.Run` through discovery, artifact parsing,
aggregation, scripted AI analysis, and output writing against local fixtures.
It has no network, model, or GCS dependency.

```bash
make e2e
```

The harness uses:

- The local storage provider with a fixture tree that mirrors Prow storage.
- `internal/aitest.ScriptServer` for ordered deterministic model responses.
- `internal/aitest.ReplayServer` for recorded request and response fixtures.

Fixtures live under `backend/internal/e2e/testdata`. Scrub secrets and private
artifact content before committing a recording.

## AI quality benchmark

The opt-in benchmark runs real agentic analysis against labeled historical
failures. Model output is nondeterministic, so it is not part of CI.

```bash
cd backend
RUN_AI_BENCHMARK=1 \
AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
AI_MODEL=<model-id> AI_TOKEN=<token-or-placeholder> \
  go test ./internal/e2e -run TestAIBenchmark -v -timeout 60m
```

Set `BENCH_PROJECT_DIR` to a consumer repository to load its prompt and AI
settings. The benchmark also accepts `BENCH_MAX_ITERS`, `BENCH_TIMEOUT`,
`BENCH_MIN_TOOL_CALLS`, `BENCH_MIN_GCS_BYTES`, and
`BENCH_CRITIQUE_RETRIES` overrides.

There is no checked-in A/B comparison command. Compare benchmark logs or saved
results when evaluating two models or configurations.

## Documentation validation

When editing Markdown:

- Verify local links and heading anchors.
- Validate generated scaffold text with `go test ./internal/onboard`.
- Run `make helm-check` when Helm templates, packaged files, examples, or values
  change. It lints the chart, verifies Orka Tool synchronization, exercises
  owned and external artifact Tool renders, and tests the operational helpers.
