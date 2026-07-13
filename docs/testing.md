# Testing

The engine has deterministic unit and end-to-end tests. Model quality evaluation
is tracked separately from CI.

## Unit tests

Per-package tests live alongside the code. Run them with:

```bash
make test
cd backend && go vet ./...
```

These are the CI gates (see `.github/workflows/ci.yml`).

## End-to-end pipeline tests

`internal/e2e` runs the full `fetcher.Run` pipeline (discover, fetch, parse
JUnit, aggregate, AI analysis, write output) against committed fixtures with no
network, no real model, and no GCS. They are hermetic and deterministic.

```bash
make e2e           # go test ./internal/e2e/... -count=1 -v
```

The harness relies on two seams:

- **Local storage provider** (`storage.ProviderLocal`): reads artifacts from a
  directory tree mirroring the bucket layout. The fixture `project.yaml` sets
  `storage.provider: local`, `storage.base: <fixture bucket>`, and
  `discovery.source: bucket`, so discovery and the agentic artifact tools both
  read the fixtures. This provider is also usable for offline or air-gapped
  fetches against a downloaded artifact tree. It is intended for testing and
  offline use, not for publishing a public dashboard: without `storage.web_base`
  set, artifact links are emitted as root-relative paths (never the on-disk
  root). Set `web_base` to the real public bucket URL if you publish
  offline-fetched data.
- **Scripted model** (`internal/aitest.ScriptServer`): an httptest
  chat-completions server that returns queued responses in order, so a single
  failure's agentic loop is fully deterministic. Its sibling
  `aitest.ReplayServer` serves recorded responses keyed by a request
  fingerprint, for higher-fidelity record/replay.

Fixtures live under `internal/e2e/testdata/`:

```
testdata/
  bucket/logs/<job>/<build>/{started,finished}.json
  bucket/logs/<job>/<build>/build-log.txt
  bucket/logs/<job>/<build>/artifacts/junit.xml
  prompts/system.md
```

To add a scenario, extend the fixture tree and add a test in
`internal/e2e/pipeline_test.go`.

### Recording real model responses

`aitest.ReplayServer` can capture fixtures from a real endpoint once, then
replay them deterministically. Build a recording server with an upstream URL and
token, run the analysis once to populate `testdata`, then switch the test to
`NewReplayServer`. Scrub any sensitive content from recorded responses before
committing.

## Quality evaluation

A separate, gated harness scores AI analysis quality against a labeled dataset
of real failures (transient precision/recall, grounding rate, citation validity,
depth) Because model output is non-deterministic, quality evaluation is not a pass/fail
CI gate. There is currently no checked-in A/B comparison command.
