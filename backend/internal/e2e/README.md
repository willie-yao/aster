# AI quality benchmark

`benchmark_test.go` scores real agentic analysis against labeled historical CI
failures, to track and guard root-cause quality as the prompts, tools, and fix
harness change. `benchmark_orka_test.go` runs the same cases and scorer through
the live Orka producer, Task, Tool, and ingestor path. Both are separate from
`pipeline_test.go`, which uses a scripted model and never calls a real endpoint.

## What it does

For each case in `benchCases`, the in-process benchmark runs `ai.Service` with
the filesystem and k8s tools against a real build's artifact tree. The Orka
adapter builds a one-failure dashboard skeleton, applies the production
producer's scoped Tools and Task, waits through the production ingestor, and
scores the patched result. Both check the model output against the same `must` /
`nice` signal regexes. A missed `must` signal fails the test.

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
- `BENCH_MIN_TOOL_CALLS`, `BENCH_MIN_GCS_BYTES`, `BENCH_MAX_ITERS`,
  `BENCH_TIMEOUT`, `BENCH_CRITIQUE_RETRIES` override the default (weak-model)
  floors so a stronger model can be benchmarked fairly, since the weak-model
  floors distort a strong model that answers concisely. Example for a strong
  hosted model: `BENCH_MIN_TOOL_CALLS=3 BENCH_MIN_GCS_BYTES=0`.

## Running through Orka

The Orka adapter is also opt-in and is never part of normal tests or CI. It
requires an installed Orka control plane, a ready Provider, the compatibility
worker, the artifact Tool service, and an Orka API endpoint reachable from the
test process.

Start a local API port-forward and create a short-lived API token:

```bash
kubectl -n orka-system port-forward svc/orka 18099:8080
kubectl -n orka-system create token orka --duration=30m
```

Then run one case:

```bash
cd backend
RUN_ORKA_BENCHMARK=1 \
BENCH_ORKA_PROVIDER=<provider-name> \
BENCH_ORKA_MODEL=<model-id> \
BENCH_ORKA_API=http://127.0.0.1:18099 \
BENCH_ORKA_TOKEN=<short-lived-token> \
BENCH_ORKA_CONTEXT=<kube-context> \
  go test ./internal/e2e \
    -run 'TestOrkaAIBenchmark/flatcar-worker-dns-providerid$' \
    -v -timeout 60m
```

The test builds and invokes the production `orka-producer` and `orka-ingestor`.
It applies uniquely identified Tasks and Tools to `orka-system` by default. It
does not delete them. Use a dedicated namespace or remove only the generated
resources after reviewing their labels.

Additional options:

- `BENCH_PROJECT_DIR` uses a real consumer prompt and project configuration.
- `BENCH_ORKA_NAMESPACE` selects the Task and Tool namespace.
- `BENCH_ORKA_API_MODE` selects `auto`, `responses`, or `chat_completions`.
- `BENCH_ORKA_VERSION` sets the manual analysis-contract version.
- `BENCH_ORKA_TASK_TIMEOUT` sets the Task execution and producer wait deadline.
  `BENCH_ORKA_WAIT` sets the ingestion deadline.
- `BENCH_ORKA_TOKEN_FILE` avoids placing the API token directly in the
  environment.
- `BENCH_ORKA_TOOL_MANIFESTS` points at Tool manifests customized for a release.
- `BENCH_ORKA_TASK_EXECUTION` supplies `Task.spec.execution` placement JSON.
- `BENCH_ORKA_TOOL_AUTH_SECRET` and `BENCH_ORKA_TOOL_AUTH_KEY` select the
  artifact Tool bearer Secret.

The generated project routes artifacts to live GCS. Unlike the in-process
adapter, the in-cluster artifact Tool cannot see the fixture extracted on the
local test host. After Prow garbage collection, use `BENCH_PROJECT_DIR` with a
storage backend that the artifact Tool pods can reach, such as a gcsweb mirror
or a fixture mounted into those pods at the configured local path.

A local kind cluster can use an inference service inside another cluster by
port-forwarding that service to the host and pointing the kind Provider at the
Docker host alias. On Docker Desktop, for example:

```bash
kubectl -n default port-forward svc/kimi-0905-serve-svc 18000:8000
```

The kind Provider uses `http://host.docker.internal:18000/v1`. This isolates the
benchmark's Orka Tasks and Tools from a deployed dashboard, but it still shares
the remote model's inference capacity. Do not overlap it with a production or
evaluation refetch using the same endpoint.

## Signal tiers

Each case's `signals` are regexes checked against the model's summary, root
cause, and suggested fix. A `must` signal that misses fails the test. A `nice`
signal is informational (how deep the analysis got). Some `nice` signals are
labeled `STRETCH`: an aspirational bar even strong models miss today, tracked
but never required. Keep the `must` bar at the achievable correct diagnosis so
the benchmark is a real regression gate rather than permanently red.

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
  in cluster-api-provider-azure PR #6358, in a different repo than the job. This
  is the hard/aspirational case: the exact route-table cause is a `STRETCH`
  signal, and the `must` bar is the achievable high-level diagnosis (systemic,
  control-plane/networking, CAPZ).

- **flatcar-worker-dns-providerid**
  (`flatcar-sysext-dns-providerid.tar.gz`):
  `periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-24` build
  `2073261474372915200` from July 4, 2026. The Flatcar worker VM and Node were
  running, but the Node retained its external-cloud-provider initialization
  taint and never gained a providerID. cloud-node-manager then crash-looped
  because it could not reach the API Service ClusterIP. The initiating error is
  one artifact deeper: worker kube-proxy never synchronized because its API
  endpoint DNS lookup used `[::1]:53`, which refused the connection. Build
  `2074370797262082048` passed on July 7, 2026 with the same Kubernetes,
  Flatcar, and containerd versions. This is the middle case: it requires a short
  generic Kubernetes artifact chain, but not Azure route-table expertise. The
  cloud-node-manager/providerID chain is required; the final kube-proxy/DNS hop
  is tracked as a stretch signal.

- **apiversion-upgrade-clusterctl-aso-ratelimit**
  (`apiversion-upgrade-aso-clusterctl.tar.gz`):
  `periodic-cluster-api-provider-azure-apiversion-upgrade-main` build
  `2074603331648491520`. `clusterctl upgrade` scales the Azure Service Operator
  (ASO) controller down during the management-cluster provider upgrade, so ASO's
  CRD conversion webhook becomes unreachable. clusterctl's object-graph
  discovery then fails listing ASO resource CRDs (VirtualNetworksSubnet,
  ManagedClustersAgentPool) because the storage-version conversion call is
  refused, retrying until the client-side rate limiter hits its context
  deadline. Unlike the route-table case, the proximate cause is stated verbatim
  in `build-log.txt` and the `clusterctl-upgrade.log` dumps, so a competent agent
  finds it by reading the logs. Persistent (7+ consecutive builds); the real fix
  is partly upstream in cluster-api's clusterctl upgrade sequencing. This is the
  achievable case: a strong analysis scores full marks.

## Orka container analyzer kind smoke test

The container analyzer spike has a separate scripted local kind test. It builds
the pinned Orka controller, builds a dashboard analyzer image, downloads the
pinned Flatcar fixture, creates CPU and mock GPU node pools, tests Task retry and
result collection, then deletes the cluster and temporary resources.

Run it from the repository root:

```bash
experimental/orka/run-container-analyzer-kind.sh
```

The cluster ownership and image cleanup regression check is available separately:

```bash
experimental/orka/test-container-analyzer-kind.sh
```

The test does not call a live model and does not touch H100 or Ray resources.
