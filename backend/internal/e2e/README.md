# AI quality benchmark

`benchmark_test.go` scores the real agentic analysis against real historical CI
failures, to track and guard root-cause quality as the prompts, tools, and fix
harness change. It is separate from `pipeline_test.go`, which uses a scripted
model and never calls a real endpoint.

## What it does

For each case in `benchCases` it runs the full agentic analysis (`ai.Service`
with the filesystem + k8s tools) against a real build's artifact tree, then
checks the model's root cause against a set of `must` / `nice` signal regexes. A
missed `must` signal fails the test, so the benchmark doubles as a regression
gate.

## Running it

The benchmark is gated: it is skipped under `go test ./...` unless
`RUN_AI_BENCHMARK` is set and an endpoint is configured. It costs real model
tokens or GPU.

```bash
RUN_AI_BENCHMARK=1 \
AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
AI_MODEL=moonshotai/Kimi-K2.7-Code AI_TOKEN=x \
go test ./internal/e2e -run TestAIBenchmark -v -timeout 60m
```

Options:

- `BENCH_PROJECT_DIR=<consumer-repo>` loads that consumer's real `project.yaml`
  AI tuning and `prompts/system.md`, so the run matches that live deploy exactly.
  Without it, a compact built-in prompt and the live CAPZ-Dynamo tuning are used.
- `BENCH_USE_GCS=1` reads artifacts from live GCS instead of the committed
  fixture. Only works before Prow garbage-collects the build.

## Fixtures

Prow garbage-collects GCS artifacts on a rolling window, so each case's full
artifact tree is snapshotted and published as a `.tar.gz` asset on the
`benchmark-fixtures` release of this repo. By default the benchmark downloads
the asset, extracts it to a local cache (`os.UserCacheDir()`), and reads it
through the `local` storage provider, so the agent traverses the exact
real directory structure. The download is cached across runs.

To add a case: capture a real failing build, snapshot its full bucket-relative
tree, upload it as a release asset, and add a `benchCase` referencing the asset
plus the root-cause signals a correct analysis should contain.

### Current cases

- **ccm-dualstack-control-plane-routetable** (`ccm-dualstack-capz-6358.tar.gz`):
  `pull-cloud-provider-azure-e2e-ccm-dualstack-capz-1-30` build
  `2062345846720040960`. Failed 100% because CAPZ does not default a route table
  onto the control-plane subnet; on dual-stack Calico runs `encapsulation: None`,
  so the control plane cannot reach worker pod CIDRs, the Calico APIService goes
  unreachable, and every namespace hangs Terminating. All 64 failed tests report
  only "timed out waiting for the condition", so the agent must read the
  `AzureCluster` resource dump to find the empty control-plane route table. Fixed
  in cluster-api-provider-azure PR #6358, in a different repo than the job.
