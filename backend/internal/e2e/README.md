# End-to-end pipeline test

`pipeline_test.go` runs `fetcher.Run` through discovery, artifact parsing,
aggregation, scripted AI analysis, and output writing against committed
fixtures. It is hermetic: no network, model endpoint, or GCS dependency.

It uses the local storage provider over `testdata/bucket`, which mirrors the
Prow artifact layout, and `internal/aitest` for ordered deterministic model
responses.

```bash
make e2e
```

The opt-in quality benchmarks that used to live here now live in
`backend/benchmarks`. They call real model endpoints and are gated behind
`RUN_*` and `BENCH_*` environment variables.
