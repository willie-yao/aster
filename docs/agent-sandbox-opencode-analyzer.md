# Agent Sandbox OpenCode analyzer

Status: private deployment prototype. Helm can install its security boundary,
but the fetcher and worker cannot create analyzer workloads. It has no public
output or cache authority.

## Goal

Test whether a thin Agent Sandbox workload using OpenCode's native debugging
harness can match or improve the in-process analyzer while removing substantial
dashboard-owned agent-loop complexity.

The initial prototype has one model session and one structured result. It has no
critic, judge, evidence digest, repair request, revision pass, case-specific
rule, model-directed evidence planner, or public authority.

## File-backed input

The dashboard prepares two immutable trees:

- `source/`: a Git checkout pinned to one full commit SHA;
- `artifacts/`: the bounded failure artifact snapshot.

The source verifier accepts regular tracked files and tracked relative symlinks
whose complete symlink chain remains inside the checkout. It rejects absolute
or escaping symlinks, unsupported index flags, submodules, dirty tracked files,
and untracked or ignored files.

Artifact bounding is mechanical. Paths are safe and sorted, each file is at
most 8 MiB, the snapshot contains at most 512 files, and total bytes are at most
32 MiB. Every artifact path, size, and SHA-256 digest is sealed in the request.
There is no semantic evidence ranking or excerpt selection.


The executor also reads the bounded local OpenCode session result to aggregate private telemetry. It records request, token, cost-availability, step, tool, denial, failure, context-limit, timeout, and structured-output status only. Prompts, responses, reasoning, file contents, quotations, credentials, and raw OpenCode payloads are never persisted or printed. Missing, malformed, and truncated telemetry remain distinct from a valid zero count.

The Agent Sandbox deployment phase must mount both trees read-only. OpenCode may write only isolated runtime state under temporary storage. It returns one schema-constrained structured object. OpenCode 1.18.2 does not implement structured-output retries, so the executor does not request or infer them. The executor validates path and line ranges, reconstructs exact quotations from the sealed workspace, and writes exactly one canonical result file at `result/analysis.json`.

## Staged Agent Sandbox lifecycle

The shared Agent Sandbox runner supports one optional staged workspace:

- one immutable stager init-container image;
- one bounded, content-addressed stager request that must exactly match the
  execution manifest, source revision, build prefix, and artifact identities;
- one shared `emptyDir` workspace;
- one read-only staged workspace mount in the executor;
- one separate writable volume overlaid at `result/`;
- separate writable temporary storage for the stager and executor so staging
  credentials or state cannot cross the container boundary.

The stager receives the complete workspace mount read-write and must finish
successfully before the executor starts. The executor receives only its analysis
request. Both containers run non-root with a read-only root filesystem, dropped
capabilities, RuntimeDefault seccomp and AppArmor when available, bounded
resources, and no automatic ServiceAccount token.

The stage request, stager image, executor request, resource bounds, and workload
shape all participate in the Sandbox identity. UID-checked cleanup and bounded
Pod-log retrieval remain owned by the existing shared lifecycle.

The credential-free stager now reads one pre-populated, read-only PVC snapshot at
`/<manifest-hash>/source` and `/<manifest-hash>/artifacts`. It validates the
source revision and artifact identities, fetches only the pinned commit into a
shallow local checkout, copies the bounded artifacts, and creates an empty result
directory. The Sandbox receives no source, storage, or model credential.

PVC population and image publication remain manual operator responsibilities.

## Deployment boundary

`agentSandbox.analyzer.enabled` defaults to `false`. Enabling it creates only
the analyzer security boundary:

- one dedicated analyzer client ServiceAccount when chart-managed RBAC is enabled;
- one narrow Role and RoleBinding in a dedicated existing execution namespace;
- one tokenless analyzer workload ServiceAccount when requested;
- one fail-closed ValidatingAdmissionPolicy and binding;
- one deny-by-default network policy; and
- one ResourceQuota permitting one Sandbox and one Pod at a time.

The chart does not create the namespace, RuntimeClass, Agent Sandbox controller,
input PVC, gateway, or images. Both images require immutable SHA-256 digests.
The admission policy pins the requester, namespace, RuntimeClass, ServiceAccount,
executor and stager images, input claim, container count, mounts, resources,
AppArmor, seccomp, and delete lifecycle. The executor receives the staged
workspace read-only, a separate writable result volume, and separate temporary
storage.

The network policy denies ingress and permits only DNS plus the configured
internal gateway. `networkPolicy.gatewayPort` is the HTTPS Service port in the
gateway URL. `networkPolicy.gatewayTargetPort` is the backend Pod port enforced
after Service translation. It defaults to `gatewayPort` for existing values and
must be set to `8443` when Service port `443` targets Pod port `8443`. The gateway
must separately authenticate the analyzer
ServiceAccount. The ResourceQuota assumes the analyzer namespace is dedicated.

A gateway-side `CiliumNetworkPolicy` that selects the analyzer namespace uses
the raw `io.kubernetes.pod.namespace` label key. The `k8s:` prefix shown by
Cilium identity inspection is not part of a policy selector key.

No chart Deployment or CronJob receives analyzer environment variables or the
client ServiceAccount. Installation therefore grants no scheduled analyzer
authority. Manual validation must use the client identity explicitly.

## Native OpenCode boundary

OpenCode receives the pinned workspace, failure metadata, consumer guidance,
and one engine-owned output contract. Its native file reading, search, and edit tools remain available. Bash is denied
in the initial prototype so the executor can enforce one OpenCode session. Network access, web fetching, delegation, and
external skills are denied. Filesystem mounts and admission policy, not a
second dashboard tool loop, enforce the source and artifact boundary.

The executor runs OpenCode once. It verifies source and artifact identity before
and after the session, requires exactly one result file, and emits one bounded
result through stdout. Provider usage remains unavailable unless the runtime or
gateway reports it explicitly.

## Result contract

The result contains the existing analysis semantics:

- summary and transient classification;
- root cause, severity, and suggested fix;
- verified relevant source files;
- exact artifact path, line, and quote citations;
- exact source path, line, and quote citations;
- unresolved details.

Dashboard code strictly parses the result, rejects duplicate or unknown fields,
verifies all citations against the sealed workspace, and maps a valid result to
`ai.FailureAnalysisResult`. The prototype does not publish that mapped result.

## Authority boundary

The runtime remains private, disabled, and non-authoritative. It cannot affect:

- dashboard JSON;
- analysis caches;
- issues or fixes;
- notifications;
- corrections;
- remediation;
- resolution state.

Shadow orchestration and comparison storage remain deferred to the repeated
cold benchmark phase.

## Validation

```bash
cd backend
go test ./internal/agentanalysis ./internal/analysisexecutor ./internal/analysisstager ./cmd/analysisexecutor ./cmd/analysisstager -count=1
go test ./... -count=1
go vet ./...
staticcheck ./...
```

## Repeated cold comparison

The opt-in benchmark uses the same pinned case manifest, consumer prompt,
artifact fixture, source revision, provider path, model, and repetition number
for both runtimes. The in-process arm remains `TestAIBenchmark`. The Agent
Sandbox arm is `TestAgentSandboxAnalyzerBenchmark`.

Prepare one case before writing the input PVC:

```bash
cd backend
RUN_AGENT_SANDBOX_ANALYZER_BENCHMARK=1 \
ANALYZER_BENCH_PREPARE_ONLY=1 \
ANALYZER_BENCH_SOURCE_ROOT=<clean-source-checkout> \
ANALYZER_BENCH_PREPARED_JSON=<private-prepared.json> \
ANALYZER_BENCH_ARM_LABEL=arm-b \
BENCH_MANIFEST=internal/e2e/testdata/benchmarks/cross-project-eval.json \
BENCH_CASE=<case-id> \
BENCH_PROJECT_DIR=<pinned-consumer> \
BENCH_MODEL_LABEL=model-a \
BENCH_PROVIDER_PATH=<provider-path> \
BENCH_TRANSPORT_ID=<stable-transport-id> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_ENDPOINT=<internal-https-endpoint> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_MODEL=<model> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_PROTOCOL=openai-chat-completions-v1 \
AGENT_SANDBOX_ANALYSIS_TIMEOUT=15m \
AGENT_SANDBOX_ANALYSIS_OUTPUT_LIMIT_BYTES=262144 \
go test ./internal/e2e -run '^TestAgentSandboxAnalyzerBenchmark$' -v -count=1
```

The prepared JSON contains the manifest hash plus the exact local source and
artifact roots. Populate the analyzer input PVC at:

```text
/<manifest-hash>/source
/<manifest-hash>/artifacts
```

Use a short-lived operator populator, then remove it before applying the
one-Pod analyzer quota. The source checkout must remain at the recorded commit
with a clean index and working tree. The artifact tree must remain byte-for-byte
identical to the prepared snapshot.

Run the cold Agent Sandbox repetitions with the dedicated analyzer client
kubeconfig and the immutable deployment values:

```bash
cd backend
RUN_AGENT_SANDBOX_ANALYZER_BENCHMARK=1 \
ANALYZER_BENCH_KUBE_CONTEXT=<short-lived-client-context> \
ANALYZER_BENCH_SOURCE_ROOT=<clean-source-checkout> \
ANALYZER_BENCH_PREPARED_JSON=<private-prepared.json> \
ANALYZER_BENCH_RESULTS_JSONL=<private-sandbox-results.jsonl> \
ANALYZER_BENCH_ARM_LABEL=arm-b \
BENCH_REPETITIONS=3 \
BENCH_MANIFEST=internal/e2e/testdata/benchmarks/cross-project-eval.json \
BENCH_CASE=<case-id> \
BENCH_PROJECT_DIR=<pinned-consumer> \
BENCH_MODEL_LABEL=model-a \
BENCH_PROVIDER_PATH=<provider-path> \
BENCH_TRANSPORT_ID=<stable-transport-id> \
AGENT_SANDBOX_ANALYSIS_NAMESPACE=<execution-namespace> \
AGENT_SANDBOX_ANALYSIS_IMAGE=<executor@sha256:digest> \
AGENT_SANDBOX_ANALYSIS_STAGER_IMAGE=<stager@sha256:digest> \
AGENT_SANDBOX_ANALYSIS_STAGER_INPUT_CLAIM=<input-pvc> \
AGENT_SANDBOX_ANALYSIS_SERVICE_ACCOUNT=<tokenless-workload-sa> \
AGENT_SANDBOX_ANALYSIS_RUNTIME_CLASS=<secure-runtime-class> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_ENDPOINT=<internal-https-endpoint> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_MODEL=<model> \
AGENT_SANDBOX_ANALYSIS_MODEL_GATEWAY_PROTOCOL=openai-chat-completions-v1 \
AGENT_SANDBOX_ANALYSIS_TIMEOUT=15m \
AGENT_SANDBOX_ANALYSIS_OUTPUT_LIMIT_BYTES=262144 \
AGENT_SANDBOX_ANALYSIS_CPU_REQUEST=250m \
AGENT_SANDBOX_ANALYSIS_CPU_LIMIT=2 \
AGENT_SANDBOX_ANALYSIS_MEMORY_REQUEST=512Mi \
AGENT_SANDBOX_ANALYSIS_MEMORY_LIMIT=2Gi \
AGENT_SANDBOX_ANALYSIS_EPHEMERAL_STORAGE_LIMIT=2Gi \
go test ./internal/e2e -run '^TestAgentSandboxAnalyzerBenchmark$' -v -count=1 -timeout 60m
```

Run the in-process arm with `TestAIBenchmark`, the same case, consumer,
provider, model, and three fresh cold caches. Keep the two JSONL files private.
Then generate a content-free comparison plus separate blinded scoring packets:

```bash
python3 hack/compare-agent-sandbox-analyzer-benchmark.py \
  --inprocess <private-inprocess-results.jsonl> \
  --sandbox <private-sandbox-results.jsonl> \
  --repo . \
  --expected-pairs 9 \
  --holdout-case <case-a> \
  --holdout-case <case-b> \
  --holdout-case <case-c> \
  --blind-packets <private-blind-packets.json> \
  --blind-map <private-blind-map.json> \
  --output-json <private-comparison.json>
```

Keep the blind map from the evaluator until scores are frozen. Give the
evaluator only the blind packet document. The evaluator copies its top-level
`packet_set_sha256` into one private score file with the recorded rubric identity
and one `0` to `2` integer for every dimension:

```json
{
  "version": 1,
  "packet_set_sha256": "<copied-from-private-blind-packets>",
  "rubric_version": 1,
  "score_max": 10,
  "dimensions": [
    "diagnosis",
    "artifact_evidence",
    "claim_discipline",
    "remediation",
    "source_grounding"
  ],
  "scores": [
    {
      "packet_id": "case-id-rep-01",
      "arm": "A",
      "scores": {
        "diagnosis": 2,
        "artifact_evidence": 2,
        "claim_discipline": 2,
        "remediation": 2,
        "source_grounding": 2
      }
    }
  ]
}
```

After scores are frozen, unblind them in a separate report invocation:

```bash
python3 hack/compare-agent-sandbox-analyzer-benchmark.py \
  --inprocess <private-inprocess-results.jsonl> \
  --sandbox <private-sandbox-results.jsonl> \
  --repo . \
  --expected-pairs 9 \
  --holdout-case <case-a> \
  --holdout-case <case-b> \
  --holdout-case <case-c> \
  --blind-map-input <private-blind-map.json> \
  --blind-scores <private-blind-scores.json> \
  --output-json <private-scored-comparison.json>
```

Automatic signal scoring and independent blind scoring remain separate. The
replacement quality gate stays incomplete until the blind score set is complete.
The report also keeps validity, invalid and no-result trials, citations,
lifecycle, cleanup, latency, requests, tokens, and cost availability separate.

The simplicity criterion is explicit. The direct analyzer must retain one model
session, contain none of the forbidden critic, digest, revision, evidence
planner, or case-specific phases, and keep its dashboard-owned production lines
at or below half of the in-process analyzer's production lines. Similar quality
without this reduction is not a successful replacement.

## Purpose-built OpenCode agent

The executor configures one private primary agent named `analysis`; it does not
use OpenCode's generic coding-oriented `build` agent. The agent receives static
engine-owned diagnostic guidance and may use only native glob, grep, and StructuredOutput tools. The native read tool is disabled because OpenCode 1.18.2 can load nearby `AGENTS.md`, `CLAUDE.md`, or `CONTEXT.md` files into privileged system reminders. The agent uses bounded grep results for file content and line inspection. Shell, read, edit, write, patch, network, delegation, external skills, and external-directory access are denied by tool selection and permission rules. The executor, not the model, writes the canonical result file.

The execution request seals the configured model context and output limits and
passes those exact values to OpenCode. The analyzer has no hard-coded context
window. Benchmarks must set `ANALYZER_BENCH_MODEL_CONTEXT_TOKENS` and
`ANALYZER_BENCH_MODEL_OUTPUT_TOKENS` from the configured model. The existing
20-step bound remains the default benchmark bound; a 40-step arm is the only
larger bound planned for the corrected experiment and does not change any
production default.
