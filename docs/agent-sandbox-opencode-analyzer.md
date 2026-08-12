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

The executor also reads the bounded local OpenCode session result to aggregate private telemetry. It records request, token, cost-availability, step, tool, denial, failure, context-limit, timeout, and structured-output status only. Request-shape telemetry includes the model, pinned outbound system-prompt bytes verified by the OpenCode 1.18.2 compatibility test, user-prompt bytes, the selected native tool-schema digest, response-schema digest, streaming and tool-choice modes, model limits, and the running OpenCode version. API failures retain only the error name, HTTP status, retryability, an engine-owned classification, an allowlisted metadata code, and bounded body-presence, size, and digest facts. `UnknownError` additionally retains an allowlisted cause name and code, the message byte count and digest after URL and credential redaction, and bounded known-or-unknown lifecycle facts about provider, stream, tool, and session-persistence progress. Provider request counts are observed lower bounds with a separate exactness flag, so a later error without a persisted provider step does not turn earlier observed requests into a fabricated exact total. The before-provider and before-tool fields describe session progress, while during-stream and during-tool fields describe the failing message and remain unset when the stage is not proven. The raw message is never retained. Prompts, provider messages, response bodies, response-header values, URLs, model output, reasoning, file contents, quotations, credentials, and raw OpenCode payloads are never persisted or printed. Missing, malformed, and truncated telemetry remain distinct from a valid zero count.

The Agent Sandbox deployment phase must mount both trees read-only. OpenCode may write only isolated runtime state under temporary storage. It returns one schema-constrained structured object. OpenCode 1.18.2 does not implement structured-output retries, so the executor does not request or infer them. The executor validates path and line ranges, reconstructs exact quotations from the sealed workspace, and writes exactly one canonical result file at `result/analysis.json`.

## Content-addressed prepared workspace lifecycle

The analyzer mounts the already prepared input PVC snapshot directly:

- `/<manifest-hash>/source` is mounted read-only at `/workspace/source`;
- `/<manifest-hash>/artifacts` is mounted read-only at `/workspace/artifacts`;
- each trial receives fresh bounded `result` and executor temporary `emptyDir` volumes;
- the executor verifies source revision, artifact hashes, expected effective mount points, and read-only mount options before and after OpenCode runs. Admission binds both PVC subpaths to one immutable manifest annotation. On AKS Kata the guest exposes each subPath as a separate read-only virtiofs root, so content hashes provide the final manifest identity check.

There is no analyzer init container and no per-trial clone or artifact copy. The
manifest hash remains the content address for the source and artifact subpaths.
A separate prepared-workspace identity seals the destination filesystem mode
policy and participates in the Sandbox workload identity. Admission requires
and freezes both identities. A wrong, missing, mutable, or mismatched snapshot
fails executor verification. OpenCode state, result files, caches, and temporary
files are never shared across trials. UID-checked cleanup and bounded Pod-log
retrieval remain unchanged.

Prepared-workspace population has a fixed sealing order:

1. Populate the source and artifact trees on the final destination filesystem.
2. Verify the exact revision, tree, index modes, tracked bytes, symlink targets,
   and absence of staged, untracked, ignored, unsupported, or linked Git data.
3. Probe whether the destination can represent executable and non-executable
   regular files distinctly.
4. Keep repository-local `core.filemode=true` when it can. Otherwise set only
   repository-local `core.filemode=false` and seal
   `ignore_executable_bit` as the source mode policy.
5. Reverify the populated source, then calculate the stage, execution,
   prepared-workspace, workload, and runtime identities.
6. Mount the sealed source and artifacts read-only for analyzer execution.

The direct prepared path seals the same derived policy as its input and execution
policy. The copy-based stager seals input and output policies separately so an
Azure Files input can be cloned into a mode-preserving execution workspace.

The mode policy never changes `HEAD`, the Git index, Git tree modes, tracked
bytes, symlink targets, source revision, artifact manifest, manifest hash, or
PVC subpaths. A policy or local Git configuration change after sealing is an
identity mismatch. A mode-only mismatch on a filesystem that can preserve modes
is rejected rather than reclassified. Content changes, index-mode changes,
staged files, untracked or ignored files, unsupported modes, submodules,
escaping symlinks, and Git metadata links remain fatal.

The preserved benchmark showed a content-size-dependent pre-executor gap on
three repeated cold trials per case:

| Case | Prepared bytes | Median pre-executor gap |
|---|---:|---:|
| Secrets Store CSI | 256,763 | 20,866 ms |
| Kueue | 20,270,997 | 270,474 ms |
| GCP PD CSI | 9,772,598 | 170,985 ms |

Publication after task finalization was 29-102 ms and cleanup was 338-1,154 ms.
The old harness did not expose separate scheduling and init-container timestamps,
so the preserved record can only prove the combined scheduling-plus-staging gap.
The strong repeated size relationship and the stager's per-trial shallow clone
and copy path support repeated materialization as the leading hypothesis for that gap. The
runtime now records scheduling, staging, executor, publication, and cleanup
phases separately for the corrected benchmark.

PVC population and image publication remain manual operator responsibilities.
The benchmark preparation step probes and seals `ANALYZER_BENCH_SOURCE_ROOT` on
the final target filesystem before generating the prepared JSON. The mode policy
is derived from that probe and cannot be selected independently by an operator.
Normal execution loads that prepared JSON and verifies the sealed policy without
rewriting repository configuration or regenerating prepared identities.

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
input PVC, provider Secret, gateway, or images. Both images require immutable SHA-256 digests.
The admission policy pins the requester, namespace, RuntimeClass, ServiceAccount,
executor image, input claim, container count, mounts, resources,
AppArmor, seccomp, and delete lifecycle. The executor receives the staged
workspace read-only, a separate writable result volume, and separate temporary
storage.

The network policy denies ingress and permits only DNS plus the configured
provider. Internal provider or gateway Pods can use Kubernetes NetworkPolicy
with namespace and Pod selectors. Cilium mode uses the Kubernetes Service for
an internal endpoint or an exact `toFQDNs` rule for an external direct provider.
External direct providers therefore require Cilium mode.
`networkPolicy.gatewayPort` remains the HTTPS port in the provider URL.
`networkPolicy.gatewayTargetPort` is the backend Pod port used only after
internal Service translation. The provider or gateway must separately
authenticate the analyzer workload. The ResourceQuota assumes the analyzer
namespace is dedicated.

A gateway-side `CiliumNetworkPolicy` that selects the analyzer namespace uses
the raw `io.kubernetes.pod.namespace` label key. The `k8s:` prefix shown by
Cilium identity inspection is not part of a policy selector key.

No chart Deployment or CronJob receives analyzer environment variables or the
client ServiceAccount. Installation therefore grants no scheduled analyzer
authority. Manual validation must use the client identity explicitly.

## Provider credential boundary

Agent Sandbox remains disabled by default. After the analyzer boundary is
explicitly enabled, direct mode is the default. Direct bearer mode exposes one
dedicated inference credential to the OpenCode executor through the fixed
`PROW_AI_MODEL_PROVIDER_TOKEN` environment variable. The Secret must already
exist in the analyzer execution namespace. Direct unauthenticated mode and
explicit gateway mode render no Secret reference.

The chart never creates, copies, reads, or prints the Secret. Admission pins the
exact Secret name, key, fixed environment variable, and auth mode while
rejecting `envFrom`, Secret volumes, projected tokens, extra credentials, and
arbitrary environment entries. OpenCode configuration contains only an
environment reference, not the token. The executor rejects any exact credential
found in process streams, structured output, canonical analysis content, or
failure data before writing `result/analysis.json` or stdout.

Use only a dedicated inference credential. Do not reuse dashboard write tokens,
GitHub read credentials, OAuth credentials, or general PATs. Gateway mode keeps
the workload tokenless and requires the gateway to attach provider credentials
outside the Sandbox process.

Chat Completions maps to `@ai-sdk/openai-compatible`. Responses maps to
`@ai-sdk/openai`. With pinned OpenCode 1.18.2, Responses requires direct bearer
auth because the provider package requires an API key before it starts a
request. Direct unauthenticated access and tokenless gateway mode remain
Chat-Completions-only.

## Native OpenCode boundary

OpenCode receives the pinned workspace, failure metadata, consumer guidance,
and one engine-owned output contract. One server process and one session carry
the evidence and finalization messages. The step budget reserves two steps for
finalization. StructuredOutput is required on the first, while the spare step
keeps OpenCode 1.18.2's last-step assistant sentinel out of the provider
request. The evidence agent has bounded native read and search access. The finalization agent has only StructuredOutput. Network
access, web fetching, delegation, writes, project configuration, and external
skills remain denied. Filesystem mounts and admission policy, not a second
dashboard tool loop, enforce the source and artifact boundary.

The executor verifies source and artifact identity before and after the session,
requires exactly one result file, and emits one bounded result through stdout.
Sanitized telemetry retains full-session requests, tokens, cost availability,
steps, tool counts, failures, and denials plus the bounded phase counters. The
final synchronous structured message is combined with evidence-phase session
telemetry because OpenCode 1.18.2 does not expose the completed structured message
through the session message-list endpoint. No prompt, output, file content, raw
event, response body, or provider message is retained.

Responses uses the native streaming provider path with `store: false`. OpenCode
keeps the complete evidence, tool-call, tool-result, and finalization history in
one local session. The executor does not use `previous_response_id` or depend on
provider-side response chaining. Exact OpenCode 1.18.2 fixtures prove streaming
text, function calls, StructuredOutput, multi-turn history, actual usage when
reported, unavailable usage when omitted, and sanitized HTTP or malformed-stream
failures. These deterministic results do not establish live compatibility with
every Responses-like endpoint or model.

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
go test ./internal/agentanalysis ./internal/analysisexecutor ./internal/agentsandbox ./internal/fixruntime ./cmd/analysisexecutor -count=1
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
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_CREDENTIAL_MODE=<direct-or-gateway> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_API=<chat_completions-or-responses> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_ENDPOINT=<full-chat-completions-endpoint> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_MODEL=<model> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_TYPE=<none-or-bearer> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_SECRET_NAME=<existing-secret-if-bearer> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_SECRET_KEY=<secret-key-if-bearer> \
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
BENCH_REPETITIONS=2 \
BENCH_MANIFEST=internal/e2e/testdata/benchmarks/cross-project-eval.json \
BENCH_CASE=<case-id> \
BENCH_PROJECT_DIR=<pinned-consumer> \
BENCH_MODEL_LABEL=model-a \
BENCH_PROVIDER_PATH=<provider-path> \
BENCH_TRANSPORT_ID=<stable-transport-id> \
AGENT_SANDBOX_ANALYSIS_NAMESPACE=<execution-namespace> \
AGENT_SANDBOX_ANALYSIS_IMAGE=<executor@sha256:digest> \
AGENT_SANDBOX_ANALYSIS_STAGER_INPUT_CLAIM=<input-pvc> \
AGENT_SANDBOX_ANALYSIS_SERVICE_ACCOUNT=<tokenless-workload-sa> \
AGENT_SANDBOX_ANALYSIS_RUNTIME_CLASS=<secure-runtime-class> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_CREDENTIAL_MODE=<direct-or-gateway> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_API=<chat_completions-or-responses> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_ENDPOINT=<full-chat-completions-endpoint> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_MODEL=<model> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_TYPE=<none-or-bearer> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_SECRET_NAME=<existing-secret-if-bearer> \
AGENT_SANDBOX_ANALYSIS_MODEL_PROVIDER_AUTH_SECRET_KEY=<secret-key-if-bearer> \
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
provider, model, and two fresh cold trials. Keep the two JSONL files private.
Then generate a content-free comparison plus separate blinded scoring packets:

```bash
python3 hack/compare-agent-sandbox-analyzer-benchmark.py \
  --inprocess <private-inprocess-results.jsonl> \
  --sandbox <private-sandbox-results.jsonl> \
  --repo . \
  --expected-pairs 6 \
  --holdout-case <case-a> \
  --holdout-case <case-b> \
  --holdout-case <case-c> \
  --required-repetitions 2 \
  --blind-packets <private-blind-packets.json> \
  --blind-map <private-blind-map.json> \
  --reference-manifest backend/internal/e2e/testdata/benchmarks/agent-sandbox-causal-references.json \
  --output-json <private-comparison.json>
```

Keep the blind map from the evaluator until scores and their digest are frozen.
Give the evaluator only the blind packet document. Each packet contains the
runtime-neutral causal reference and full-credit requirements for that case. The
evaluator copies both set hashes into a version 2 score file, provides one `0` to
`2` integer per dimension, and records the causal assessment:

```json
{
  "version": 2,
  "packet_set_sha256": "<copied-from-private-blind-packets>",
  "reference_set_sha256": "<copied-from-private-blind-packets>",
  "rubric_version": 2,
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
      },
      "causal_assessment": {
        "alignment": "aligned",
        "initiating_cause_found": true,
        "downstream_treated_as_primary": false,
        "required_chain_coverage": ["required-link-a", "required-link-b"]
      }
    }
  ]
}
```

Freeze the exact score digest before disclosing the runtime map:

```bash
python3 hack/freeze-agent-sandbox-blind-scores.py \
  --blind-packets <private-blind-packets.json> \
  --blind-scores <private-blind-scores.json> \
  --output <private-score-freeze.json>
```

Only after the freeze file exists should the evaluator's scores be unblinded:

```bash
python3 hack/compare-agent-sandbox-analyzer-benchmark.py \
  --inprocess <private-inprocess-results.jsonl> \
  --sandbox <private-sandbox-results.jsonl> \
  --repo . \
  --expected-pairs 6 \
  --holdout-case <case-a> \
  --holdout-case <case-b> \
  --holdout-case <case-c> \
  --required-repetitions 2 \
  --blind-map-input <private-blind-map.json> \
  --blind-scores <private-blind-scores.json> \
  --score-freeze <private-score-freeze.json> \
  --reference-manifest backend/internal/e2e/testdata/benchmarks/agent-sandbox-causal-references.json \
  --output-json <private-scored-comparison.json>
```

Diagnosis score 2 is rejected unless every required causal link is covered, the
initiating cause is found, the assessment is reference-aligned, and downstream
noise is not presented as primary. The scored report retains packet, reference,
and score set hashes.

Automatic signal scoring and independent blind scoring remain separate. The
replacement quality gate stays incomplete until the blind score set is complete.
The report also keeps validity, invalid and no-result trials, citations,
lifecycle, cleanup, latency, requests, tokens, and cost availability separate.

The simplicity criterion is explicit. The direct analyzer must retain one model
session, contain none of the forbidden critic, digest, revision, evidence
planner, or case-specific phases, and keep its dashboard-owned production lines
at or below half of the in-process analyzer's production lines. Similar quality
without this reduction is not a successful replacement.

## Purpose-built OpenCode agents

The executor configures two private primary agents and uses them in one OpenCode
session. It does not use OpenCode's generic coding-oriented `build` agent. The
`analysis-evidence` agent receives static engine-owned diagnostic guidance and
can use native glob, grep, and read against the sealed `source/` and `artifacts/`
trees. StructuredOutput is denied and no response format is attached during this
phase. Shell access is limited to the exact read-only commands
`git status --short`, `git log -1 --oneline`, and
`git diff --no-ext-diff --stat`.

The executor fetches sanitized session telemetry after the evidence message and
requires at least one successful artifact file read or matching artifact grep.
It records successful source evidence separately. If that gate passes, the
`analysis-finalize` agent receives a second message in the same session. Its only
allowed tool is the exact schema-backed StructuredOutput function. Any native
tool attempt or any result other than exactly one StructuredOutput call fails
closed. Source citations or relevant files are rejected unless the evidence phase
recorded a successful source read or matching source grep.

Tracked or artifact `AGENTS.md`, `CLAUDE.md`, and `CONTEXT.md` files are rejected
before OpenCode starts, so nearby instruction discovery cannot elevate workspace
content. Edit, write, patch, arbitrary shell, network, delegation, external
skills, project configuration, and external-directory access remain denied in
both phases. The executor, not the model, writes the canonical result file.

The execution request seals the configured model context and output limits and
passes those exact values to OpenCode. The analyzer has no hard-coded context
window. Benchmarks must set `ANALYZER_BENCH_MODEL_CONTEXT_TOKENS` and
`ANALYZER_BENCH_MODEL_OUTPUT_TOKENS` from the configured model. The existing
20-step bound remains the default benchmark bound; a 40-step arm is the only
larger bound planned for the corrected experiment and does not change any
production default.

### AKS Kata prepared-mount smoke test

On August 11, 2026, a temporary `prow-dashboard-demo` Sandbox using
`kata-vm-isolation` successfully mounted both content-addressed PVC subpaths.
UID 65532 read both inputs, writes to each input failed with `Read-only file
system`, fresh result and temporary volumes were writable, and the Pod completed
successfully. Kata exposed the mounts as separate read-only `virtiofs` roots at
`/workspace/source` and `/workspace/artifacts`; it did not expose the host-side
subPath in the guest mountinfo root. The verifier accepts this exact Kata shape
while admission and content verification bind the mounted data to the manifest.
The temporary Sandbox, Pod, PVC, NetworkPolicy, and namespace were deleted and
namespace absence was verified. Private operator evidence retains the Sandbox, Pod, mountinfo, write-denial, and cleanup records.
