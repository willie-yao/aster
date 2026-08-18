# AI quality benchmark

This package holds the opt-in quality benchmarks. They score real agentic
analysis against labeled historical CI failures as prompts, tools, and the
harness change. Every benchmark here is gated behind its own `RUN_*` or
`BENCH_*` environment variable and skips by default, so `go test ./...` compiles
them but runs none of them.

They live outside `internal/e2e` on purpose: that package now holds only
`pipeline_test.go`, the hermetic regression test that uses a scripted model and
never calls a real endpoint.

## What it does

For each case in `benchCases`, the benchmark runs `ai.Service` with the
filesystem and Kubernetes tools against a real build's artifact tree. It checks
the model output against `must` and `nice` signal regexes. A missed `must` signal
fails the test. Each trial also reports the unique successful filesystem and
Kubernetes Tool names and their call counts from the current private trace
format.

## Running it

The benchmark is gated: it is skipped under `go test ./...` unless
`RUN_AI_BENCHMARK` is set and an endpoint is configured. It costs real model
tokens or GPU.

```bash
RUN_AI_BENCHMARK=1 \
AI_ENDPOINT=http://127.0.0.1:8000/v1/chat/completions \
AI_MODEL=moonshotai/Kimi-K2.7-Code AI_TOKEN=x \
go test ./benchmarks -run TestAIBenchmark -v -timeout 60m
```

The frozen cross-project evaluation cohort is available as an external manifest.
Run each case separately with its matching consumer. The manifest validates the
consumer commit plus `project.yaml` and `prompts/system.md` hashes before making
provider requests:

```bash
manifest="$PWD/benchmarks/testdata/benchmarks/cross-project-eval.json"

run_case() {
  RUN_AI_BENCHMARK=1 BENCH_MANIFEST="$manifest" \
  BENCH_CASE="$1" BENCH_PROJECT_DIR="$2" \
  AI_ENDPOINT=<chat-completions-url> AI_MODEL=<model> AI_TOKEN=<token> \
  go test ./benchmarks -run TestAIBenchmark -v -timeout 90m
}

run_case secrets-store-csi-image-scan /path/to/secrets-store-csi-prow-dashboard-eval
run_case kueue-was-podgroup-api-mismatch /path/to/kueue-aster-eval
run_case gcp-pd-csi-windows-mount-visibility /path/to/gcp-pd-csi-prow-dashboard-eval
```

The pinned baseline consumer commits are:

- Secrets Store CSI: `cf63e830080f203fbda95a3077c5e02da55fb6f1`
- Kueue: `e4257c64fc9c5344b01919488fc76aa3fb0618b7`
- GCP PD CSI: `f74fc047a1f6de10eec334207c4e58ce743bdcac`

Its Secrets Store CSI and Kueue cases require a grounded diagnosis. The GCP PD
CSI reference is medium confidence, so that case also accepts the engine's
grounded-policy unavailable result instead of rewarding an unsupported owner.

Options:

- `BENCH_PROJECT_DIR=<consumer-repo>` loads that consumer's real `project.yaml`
  AI tuning and `prompts/system.md`, so the run matches that live deploy exactly.
  Without it, a compact built-in prompt and the live CAPZ-Dynamo tuning are used.
- `BENCH_VARIANT_DIR=<consumer-variant>` evaluates a prompt or recipe variant
  while keeping `BENCH_PROJECT_DIR` as the immutable pinned baseline. The
  variant must have a byte-identical `project.yaml`; only the effective
  `prompts/system.md` and `skills/*.yaml` inputs may change. Set a stable
  `BENCH_ARM` whenever a variant is used. The baseline arm defaults to
  `baseline`.
- `BENCH_CASE=<case-id>` selects one exact external-manifest case. Pinned
  cross-project consumers require this so one project's prompt is never applied
  to another project's fixture.
- External manifest cases may set `test_source: build` for a Prow build-level
  failure that has no JUnit case. This preserves the production failure signal
  and build-specific floor policy.
- `BENCH_USE_GCS=1` reads artifacts from live GCS instead of the committed
  fixture. Only works before Prow garbage-collects the build.
- `BENCH_REPETITIONS=<count>` runs consecutive logical repetitions. Set
  `BENCH_REPETITION_START=<index>` when an isolated operation must retain its
  planned repetition number instead of restarting at 1.
- `AI_CACHE_GENERATION=<value>` applies the same validated, hashed cache-key
  namespace used by production.
- `BENCH_CACHE_DIR=<private-dir>` stores each case and repetition under a
  deterministic isolated subdirectory. The harness rejects a pre-existing
  `ai_cache.json` so a requested cold operation cannot silently become warm.
- `BENCH_VERIFY_CACHE_REUSE=1` saves the analysis cache, reloads it with a new
  client, and evaluates the exact current cache policy without a provider call.
  The private JSONL result separately records whether persistence was attempted
  and accepted, the policy rejection reason, whether lookup was attempted and
  accepted, the lookup rejection reason, restored floor markers, whether a
  hard-policy unavailable cooldown was found, and a provider-request count of
  zero.
- `BENCH_MIN_TOOL_CALLS`, `BENCH_MIN_GCS_BYTES`, `BENCH_MAX_ITERS`,
  `BENCH_TIMEOUT`, `BENCH_CRITIQUE_RETRIES` override the default (weak-model)
  floors so a stronger model can be benchmarked fairly, since the weak-model
  floors distort a strong model that answers concisely. Example for a strong
  hosted model: `BENCH_MIN_TOOL_CALLS=3 BENCH_MIN_GCS_BYTES=0`.
- The harness derives and enforces the maximum provider requests admitted by the
  exact agentic configuration. The cap includes the configured loop, the single
  byte-floor extension, forced finalization, one bounded critique repair, and
  semantic review. Transport retries count through each trace event's `attempts`
  value, and a truncated trace fails closed because request usage would be
  incomplete. Private JSONL records `provider_request_cap`, logical
  `model_requests`, actual `provider_attempts`, and `trace_truncated`.

For the Claude hard-policy production-readiness matrix, the fixed configuration
uses `max_iters: 11`, a non-zero byte floor, one critique retry, and semantic
review. Its exact maximum is:

```text
11 configured iterations + 1 byte-floor extension = 12 main-loop requests
+ 1 forced finalization
+ 1 optional critique Tool turn + 1 critique finalization
+ 1 semantic judge + 1 semantic refinalization
= 17 provider requests per operation

2 compatibility requests + 4 x 17 = 70 total Claude requests
```

The selected operation cap is 17. Transport retries consume that same cap rather
than receiving extra headroom. A larger value would hide an unbounded or
unexpected runtime path rather than provide legitimate headroom.

## Telemetry

Each completed analysis reports the configured quality-gate result and bounded
usage counters. The output includes evidence-plan coverage, GCS-floor bypass,
critique status and version, a short skill-set hash prefix, budget exhaustion,
semantic-judge flags, context truncations, model and Tool failures, model
requests, and provider-reported input and output tokens. Private JSONL output
also records the derived provider-request cap, GCS bytes, floor markers, sorted safe Tool counts, floor-nudge
reasons, the hashed cache generation, zero-request cache reload results, and
content-free draft metadata. Draft metadata contains stable critique rule IDs,
matched skill IDs, applicable missing or unavailable evidence-group IDs, and the
selected attempt. It records both raw findings and the findings that survive
deterministic publication sanitization. It also records every best/fallback
replacement decision with evidence revisions, strict-dominance state, and a
stable acceptance or rejection reason. Decision events displace older ordinary
trace events if the per-analysis cap is full. It does not contain draft text.

Human review uses rubric version 2 with five dimensions scored from 0 to 2:
diagnosis, artifact evidence, claim discipline, remediation, and source
grounding. The maximum human score is 10. Every private JSONL row records the
rubric version and maximum so report generation cannot describe the same totals
with a different denominator.

Every private JSONL row also records the experiment arm, engine commit, fixture
digest, pinned baseline consumer commit, effective project and prompt digests,
merged skill-set hash, API mode, a sanitized provider-configuration digest,
frozen pricing, evidence condition, frozen-evidence digest when present, and one
effective-input digest. Persistent cold-cache paths include the arm and
effective-input digest, so separate arms and evidence conditions cannot silently
share an analysis cache.

The trace summary reports the floor-nudge count and ordered reasons, context
compaction and over-budget counts, the final semantic-judge event outcome,
critique and evidence retries, and accepted-uncached events. Successful Tool
names and per-Tool counts remain sorted. If an analysis fails before producing
`AIAnalysis`, the benchmark prints the available trace and Tool summaries before
failing the test.

External cases may define bounded `evidence_groups` containing path, content,
and scoring-only root-cause regexes. Before a provider request, the benchmark
scans the pinned fixture and records whether each required signal exists. During
the trial it separately records candidate-path selection, decisive excerpt
delivery, model receipt, final citation, and root-cause-only causal use.
`evidence_group_sources` distinguishes native model Tool calls (`model_tool`),
deterministic critique repair (`repair_injection`), and a prepared frozen
benchmark bundle (`oracle_prompt`). An oracle excerpt counts as model receipt
only after the trace shows that a model request containing the prepared prompt
was made. An untagged
test-only read is `unknown` and does not count as model receipt. The private
JSONL stores only content-free group states and never stores matching content.

Draft telemetry records which supported causal facts a critique, evidence, or
semantic retry retained, added, or dropped. It does not claim per-draft citation
retention because the benchmark observer does not receive draft citations.
`trial_status` distinguishes `valid_result`, `no_result`, `invalid_result`,
`contract_violation`, `timeout`, and `runtime_failure`. A parseable safe result
remains `valid_result` when a bounded finalization repair or other contract
warning occurred. The separate `contract_violation` boolean preserves that
telemetry without converting displayable analysis into a lifecycle failure. The
JSONL row is written before a failing trial stops the test.

Both arms record `structured_valid`, `displayable`, `analysis_disposition`, and
`grounded` separately. `preliminary` means safe structured content with unresolved
evidence or quality warnings. The full evidence-contract result remains a stricter
grounding and causal-alignment dimension. A miss there does not retroactively make
the runtime result malformed. Action eligibility is not a benchmark quality metric;
it requires authenticated request-time policy and confirmation outside either arm.

`BENCH_EVIDENCE_CONDITION` defaults to `fixture-v1`. The benchmark-only
`kueue-oracle-v1` condition is available only for the pinned Kueue API-version
case. It extracts a compact line-centered bundle from the verified fixture,
checks the committed bundle hash, and gives those raw artifact lines to the
in-process model. Preparation has one 30-second deadline and a 128 MiB aggregate
scan budget. Scoring names, regexes, and the reference diagnosis are not included
in the model-visible bundle. The evidence-stage configuration is hashed, and
its expected sorted IDs are paired across comparison records so missing stages
fail closed. This condition
changes only benchmark input and identity. It does not change production
analysis behavior.

JSONL also separates `diagnosis_signal_hits` from transient and forbidden-claim
policy checks. This prevents a placeholder or abstaining answer from appearing
moderately successful merely because it avoids forbidden claims.

The telemetry never prints prompts, model response text, Tool arguments, Tool
output, endpoints, model coordinates, credentials, or full hashes.

Each frozen case declares an `evidence_mode`. `artifact_only` requires artifact
evidence and canonical artifact citations but does not require a repository read.
`artifact_and_source` additionally freezes expected source paths and source-backed
diagnosis signals. It requires a successful source read or grep, verified canonical
citations for every expected path, and all source-backed signals. Source citations,
relevant files, or source-backed claims remain invalid without source evidence
regardless of the case mode. The exact six-trial comparison reports both categories
separately and is incomplete when either category is absent.


To separate retrieval from reasoning on the Kueue case, run the same cold trial
with the frozen oracle condition:

```bash
BENCH_EVIDENCE_CONDITION=kueue-oracle-v1 \
RUN_AI_BENCHMARK=1 \
AI_API=chat_completions \
AI_ENDPOINT=<endpoint> \
AI_MODEL=<model> \
AI_TOKEN=<available-in-environment> \
BENCH_MODEL_LABEL=<anonymous-label> \
BENCH_PROVIDER_PATH=<provider>/<model> \
BENCH_TRANSPORT_ID=<transport-id> \
BENCH_MANIFEST="$PWD/benchmarks/testdata/benchmarks/cross-project-eval.json" \
BENCH_CASE=kueue-was-podgroup-api-mismatch \
BENCH_PROJECT_DIR=/private/bench/kueue-consumer \
BENCH_CACHE_MODE=cold \
BENCH_RESULTS_JSONL=/private/bench/kueue-oracle.jsonl \
go test ./benchmarks -run TestAIBenchmark -v -timeout 60m
```

A miss with `model_received_evidence=true` is evidence of a reasoning or causal
synthesis failure. A miss with that field false remains a retrieval failure.
Verify the pinned extraction without a provider by running:

```bash
RUN_BENCHMARK_FIXTURE_VALIDATION=1 \
go test ./benchmarks -run TestKueueOracleEvidenceFixture -v -count=1
```

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
historical `benchmark-fixtures` release in `willie-yao/prow-ai-dashboard`. By
default the benchmark downloads
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

## Agent Sandbox analyzer benchmark

`TestAgentSandboxAnalyzerBenchmark` is an opt-in repeated cold benchmark for the
private OpenCode workspace analyzer. It requires one pinned external benchmark
case, a clean source checkout, a pre-populated analyzer input PVC, and the
short-lived analyzer client kubeconfig. Use
`hack/compare-agent-sandbox-analyzer-benchmark.py` to pair its private JSONL with
`TestAIBenchmark` output and generate the content-free comparison.

The CAPZ cohort is pinned in
`testdata/benchmarks/capz-agent-sandbox-eval.json`. It contains the route-table,
Flatcar DNS/providerID, and clusterctl/ASO cases plus exact consumer, fixture,
source, prompt, and project identities. Set `BENCH_MODEL_CONTEXT_TOKENS` and
`BENCH_MODEL_OUTPUT_TOKENS` identically for both arms. When private JSONL is
enabled, the in-process client sends that explicit output cap; normal production
callers retain the provider default. The route-table case also freezes two
source expectations, so the nine-pair matrix contains both artifact-only and
artifact-plus-source trials.

Prepare one case without provider access by mounting the private analyzer input
claim and setting `ANALYZER_BENCH_PREPARE_ONLY=1`,
`ANALYZER_BENCH_INPUT_ROOT=<mounted-claim>`, and the exact standalone source
clone. Preparation copies the complete frozen build tree, writes a compact
hashed artifact index, and seals the request. Large cases are never reduced to
a favorable subset.

After immutable executor and stager digests are resolved, run
`hack/test-agent-sandbox-analysis-images.sh` with its optional output path to
create a private image-contract JSON. Repeat the prepare-only pass with
`ANALYZER_BENCH_IMAGE_CONTRACT_JSON` set. Scored execution rejects a prepared
record, runtime image, embedded Aster revision, UID/GID, image tag, Go version,
or OpenCode version that differs from that contract.

Blinded packets require
`--reference-manifest backend/benchmarks/testdata/benchmarks/agent-sandbox-causal-references.json`.
The packet set includes one runtime-neutral causal reference and full-credit rubric
per case. Keep `blind-map.json` withheld until `blind-scores.json` is frozen. A
score file uses version 2 and must include the packet and reference set hashes,
plus a causal assessment for every arm. Diagnosis score 2 is rejected unless the
assessment marks the initiating cause found, covers every required causal link,
does not promote downstream noise to primary cause, and is reference-aligned.
Automatic signals, structured validity, citation verification, lifecycle, and
blinded causal scores remain separate metrics.
Freeze the blinded score file with
`hack/freeze-agent-sandbox-blind-scores.py` before reading the runtime map, then
pass the resulting `--score-freeze` file to the scored comparison. The comparison
rejects a changed post-unblinding score file. The score document must contain one
UTC `scoring_timestamp`; the freeze binds that timestamp with the packet,
reference, and score hashes.

The scored report uses only these classifications:

- `insufficient_evidence`
- `inprocess_preferred`
- `shadow_promising_for_more_evaluation`
- `shadow_materially_better`

It reports every case and distribution separately. It never recommends replacing
the authoritative analyzer. `shadow_materially_better` requires no lifecycle,
validity, citation, or source-grounding regression, repeated blinded causal
improvement in more than one case, complete request/token/cost telemetry, bounded
cost and latency, and complete cleanup.

The provider-free analyzer integrity harness lives in
`internal/analysisexecutor`. It is opt-in, uses a deterministic loopback TLS
Chat Completions server, and runs inside the exact analysis executor image for
Azure Files and Kata differential checks. Its JSON summary contains only
content-free source integrity snapshots and aggregate tool telemetry.

The provider-free evidence-handle scale harness is also in
`internal/analysisexecutor`. It runs exact OpenCode 1.18.2 against a
deterministic loopback TLS gateway and representative large source and artifact
trees. Broad directory greps reproduce high-cardinality range handling. Its
summary contains only bounded counts, status, truncation, usage availability,
phase totals, and allowlisted warning or rejection codes.
