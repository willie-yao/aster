# Agent Sandbox OpenCode analyzer

> **Status: active private shadow evaluation, disabled by default.** The
> authoritative in-process analyzer publishes first. The Agent Sandbox shadow may
> then evaluate a bounded sample and write a private comparison ledger. It cannot
> replace public analysis, populate the normal cache, or authorize an action.

This page is for maintainers evaluating the OpenCode analyzer boundary. Platform
installation and exact Helm values remain in
[Kubernetes platform setup](../kubernetes-platform.md) and the
[Kubernetes operator reference](../kubernetes-reference.md).

## Authority boundary

The shadow has no authority over:

- `dashboard.json`, `jobs/*.json`, or `flakiness.json`;
- the normal analysis cache or cache acceptance policy;
- recurring-pattern publication;
- notifications, issues, Fix PRs, remediation, or resolution state.

A Sandbox failure, malformed result, timeout, ledger error, or cleanup delay must
leave the authoritative result unchanged. The shadow runs only after the
in-process refresh has a frozen result to compare.

## Workspace and evidence contract

Each request uses immutable prepared input:

```text
source/       tracked source at one full commit SHA
artifacts/    bounded failure artifacts
request/      one bounded execution request mounted read-only
result/       one writable canonical result location
tmp/          isolated executor state
```

Source verification accepts regular tracked files and tracked relative symlinks
whose complete chain remains inside the checkout. It rejects absolute or escaping
symlinks, submodules, unsupported index flags, dirty tracked files, and untracked
or ignored files. Before either scheduled clone, the public GitHub tree API must
prove no more than 100,000 files, 64 MiB per file, and 1 GiB of tracked content.
The complete prepared source snapshot, including Git metadata, is capped at 1.5
GiB.

Artifacts are mechanically bounded and content-addressed. Paths are safe and
sorted; each file, total file count, total bytes, size, and SHA-256 digest are
sealed into the request. The current contract admits at most 5,000 files, 64 MiB
per file, and 512 MiB total. The CAPZ fixtures fit without selecting a favorable
subset.

The explicit benchmark pre-populates a private input claim. Scheduled shadowing
cannot mount dashboard-local storage across namespaces. It instead retains a
local verification copy and creates a tokenless publisher Job in the dedicated
analysis namespace. The Job fetches only the sealed public source revision and
artifact paths, verifies every size and digest, and writes one leased
content-addressed snapshot to the private input claim. The publisher Job and Pod
are both confirmed absent before the Sandbox starts. Ambiguous Job creates are
recovered by deterministic name and exact workload identity, then deleted.

The input claim is mounted read-only in the Sandbox. The credential-free stager
receives the sealed execution request in 16 fixed Base64 chunks of at most 64 KiB,
reconstructs and validates the original request hash, cross-checks it against the
stage request, and writes `request.json` with exclusive no-follow creation into a
dedicated 1 MiB `emptyDir`. The executor receives that volume read-only and no
longer receives the full request in an environment variable. The stager then
verifies the compact artifact-index identity and copies each artifact once while
checking its sealed size and digest. The executor independently verifies the
materialized workspace before the first provider request and again after
OpenCode runs. Only `result/` and temporary executor state are writable. The
dashboard validates the returned result again against its retained local copy. A
second tokenless, no-network Job deletes exactly the leased remote snapshot.

Preparation, remote publication, model execution, and cleanup use separate
bounded contexts. The full configured analysis timeout starts only after both
workspace copies are ready. Comparison mode applies the same explicit output
token cap from `ai.maxOutputTokens` to the authoritative in-process client. The
chart requires it to equal the shadow `modelOutputTokens`; zero remains the
normal production default when shadowing is disabled.

OpenCode can use bounded native read, glob, and grep operations. Network fetch,
repository writes, arbitrary shell, delegation, project configuration, and
external skills are denied. The analyzer uses an Aster-pinned OpenCode 1.18.2
binary whose single source patch makes `OPENCODE_DISABLE_PROJECT_CONFIG` also
disable dynamic `AGENTS.md`, `CLAUDE.md`, and `CONTEXT.md` discovery during native
reads. Those files remain byte-identical ordinary source or artifact content.
They reach the model only when the model explicitly reads them, never as ambient
instructions attached to an adjacent read.

Successful content-bearing reads produce engine-issued evidence handles. The
model selects handles in the structured result instead of authoring citation
paths or quotes. The executor reconstructs canonical paths, line ranges, and
quotations from the sealed workspace. If the evidence agent receives one
non-retryable API 400 exactly at its allocated final request, the executor may
continue into the separately reserved source-correction and finalization phases
only when telemetry proves accepted artifact evidence, no denied tool, and no
structured-output attempt. This is not a provider retry. The original failure,
allocated and consumed steps, and recovery decision remain in private telemetry.
Every earlier, retryable, context, policy, or ungrounded failure still stops the
run. A result without valid artifact grounding remains private and preliminary
when the structured content is otherwise safe.
Source-required cases without accepted source grounding are also preliminary.
These results never gain publication, cache, fallback, action, correction,
remediation, or Fix authority.

## Executor and stager identities

The shadow uses separate immutable images and identities:

- `analysisstager` prepares one sealed workspace snapshot.
- `analysisexecutor` verifies the pinned OpenCode runtime manifest and binary
  digest, runs the analyzer, and writes exactly one canonical result.
- Scheduled shadowing also runs `analysisstager` as the namespace-local publisher
  and cleanup image. Those Jobs receive no provider, OAuth, bot, or GitHub
  credential.
- The Aster writer creates and observes the Sandbox through a dedicated client
  ServiceAccount.
- The Sandbox Pod uses a separate tokenless workload ServiceAccount.

The executor image records the upstream OpenCode 1.18.2 commit, source archive,
frozen models.dev snapshot, pinned Web UI and musl compiler builders, Aster
runtime patch, build-only target-selection patch, embedded Web UI digest, and
final binary digest. The executor verifies the manifest and binary before any
provider request. Runtime and build patches are stored under `hack/patches/`; the
compressed models catalog is under `hack/opencode/`.

Admission pins the stager and executor digests, RuntimeClass, ServiceAccounts,
PVC identities, read-only mounts, resource bounds, security contexts, provider
coordinates, and complete environment shape. Separate admission covers the
publisher and cleanup Jobs. The chart renders default deny, exact publisher and
model egress, quota, and narrowly scoped Job RBAC. It does not create the
execution namespace, Agent Sandbox controller, RuntimeClass, provider Secret or
gateway, private input PVC, or runtime images.

Use only a dedicated inference credential. Direct bearer mode references one
existing Secret key in the execution namespace. The chart and Aster processes do
not read or print the value. Gateway mode keeps the executor tokenless and
requires gateway-side workload authorization. Network reachability alone is not
authentication. The shared API, endpoint, authentication, reasoning-effort, and
TLS constraints are in
[AI providers](../ai-providers.md#agent-sandbox-provider-compatibility).

## Result contract

The structured model result contains:

- summary, root cause, severity, transient classification, and suggested fix;
- selected artifact and optional source evidence handles;
- relevant source-file handles;
- unresolved details.

The executor replaces every selected handle with verified canonical evidence and
then validates the result. Malformed JSON, unsafe paths, impossible ranges,
workspace mutation, credential exposure, output overflow, and unexpected files
are terminal failures. Missing artifact or source grounding, dropped duplicate
handles, canonicalized classification conflicts, and other safe quality defects
produce a preliminary result with bounded warning codes.

An empty suggested fix, no source citation, or no relevant source file is valid
when the evidence supports only analysis. The shadow is not a remediation stage.
Accepted results map to the normal analysis semantics only for private comparison;
they are not published.

## Isolated evaluation procedure

### 1. Validate the local contracts

```bash
cd backend
go test ./internal/agentanalysis ./internal/analysisexecutor \
  ./internal/analysisstager ./internal/analysispublisher ./internal/agentsandbox \
  ./cmd/analysisexecutor ./cmd/analysisstager -count=1
go test ./... -count=1
go vet ./...
staticcheck ./...
```

Run the provider-free source-integrity and evidence-handle harnesses before any
provider-backed comparison. They exercise the pinned OpenCode path, read-only
workspace, structured output, large evidence sets, source verification, and
content-free failure reporting without transmitting repository data externally.

### 2. Enable a bounded shadow deployment

Set `agentSandbox.analysisShadow.enabled=true` only after the platform owner has
provided and accepted:

- an existing dedicated execution namespace;
- an immutable executor image digest;
- an immutable stager image digest;
- an immutable git-capable dashboard image digest for public snapshot discovery;
- a secure RuntimeClass for hostile repository code;
- tokenless workload and dedicated client ServiceAccounts;
- the required deny-by-default network policy;
- an existing private analysis-input PVC and private shadow-ledger PVC, both
  distinct from the dashboard data PVC;
- reviewed provider coordinates and a dedicated Secret or authenticated gateway;
- exact publisher FQDNs, model context and output limits, and conservative
  `maxPerRun`, timeout, turn, output, quota, and resource limits.

Keep `retries: 0`. Start with `maxPerRun: 1`. The in-process analyzer must remain
enabled and authoritative.

### 3. Run matched cold comparisons

Use the same pinned case manifest, consumer prompt, source revision, artifact
snapshot, provider path, model, reasoning effort, context limit, output limit,
and repetition count for both arms. Run the in-process `TestAIBenchmark` arm and
the Agent Sandbox `TestAgentSandboxAnalyzerBenchmark` arm from fresh state. Keep
raw JSONL, prepared workspace metadata, blind packets, maps, and scores private.

The Agent Sandbox arm must pin both runtime images and their embedded identity:

```bash
AGENT_SANDBOX_ANALYSIS_IMAGE='<executor>@sha256:<digest>' \
AGENT_SANDBOX_ANALYSIS_STAGER_IMAGE='<stager>@sha256:<digest>' \
ANALYZER_BENCH_IMAGE_CONTRACT_JSON='<private-image-contract.json>' \
BENCH_MODEL_CONTEXT_TOKENS='<frozen-context-limit>' \
BENCH_MODEL_OUTPUT_TOKENS='<frozen-output-limit>' \
go test ./benchmarks -run TestAgentSandboxAnalyzerBenchmark -count=1
```

Generate the private image contract with
`hack/test-agent-sandbox-analysis-images.sh` after resolving immutable image
digests. The contract binds both digests, OCI revisions, embedded `--version`
output, runtime UID/GID, image tag, Go versions, upstream OpenCode commit and
source digest, frozen models.dev digest, builder identities, Aster patch identity,
and final OpenCode binary digest under one SHA-256. The image test also runs the
exact binary against a loopback instruction-file canary before writing the
contract. A final prepare-only pass must include the same contract before scored
execution.

The in-process arm uses the same two `BENCH_MODEL_*` values directly. It does not
perform provider-side context detection during a scored run.

The comparison tooling must keep these dimensions separate:

- runtime validity and no-result trials;
- diagnosis and causal-chain quality;
- artifact and source grounding;
- citations and claim discipline;
- lifecycle, latency, provider requests, token metadata, and cost availability;
- Sandbox finalization and cleanup.

Blind scoring must be frozen before the runtime map is disclosed. Do not retry a
provider case for a more favorable result, switch models between arms, or treat
missing usage metadata as zero.

### 4. Inspect and clean up

The writer appends one bounded private record to the separate shadow ledger. A
record includes authoritative and shadow identities, result status, validation
codes, comparison fields, attempts, timing, publisher and cleanup state, and
content-free telemetry. Confirm that no shadow data appears under `/data/*` or
any server API.

After each evaluation, verify publisher Job deletion before Sandbox creation,
Sandbox and Pod termination, result observation, leased-input cleanup Job
deletion, local workspace cleanup, and namespace quota recovery. A
cleanup-pending record is a failed lifecycle result, not a successful analysis.
Retain only the private artifacts required by the evaluation protocol and the
operator's evidence policy.

## Private telemetry

Telemetry may retain bounded counts and statuses for provider requests, token and
cost availability, steps, tools, denials, context limits, structured-output
status, bounded evidence exhaustion, exact duplicate successful read ranges,
validation, and cleanup. Duplicate-read telemetry is observational and does not
change the model-visible tool result. Telemetry may also retain safe model and
protocol identity plus digests of schemas and request shape.

It must not retain or print:

- prompts, model text, reasoning, or provider bodies;
- source or artifact content, paths, quotations, or tool arguments;
- Secret values, credential hashes, URLs containing credentials, or raw OpenCode
  payloads;
- unrestricted runtime logs.

Missing, malformed, truncated, and reported-zero metadata remain distinct.
Provider request counts are lower bounds unless the transport proves an exact
count.

## Cleanup and limitations

- The shadow is sampled, private, and non-authoritative. It is not a rollout path
  for replacing the in-process analyzer.
- The workspace contains the complete frozen fixture within explicit safety
  bounds. It does not reproduce a live or unbounded artifact browser.
- Pinned OpenCode protocol fixtures prove the tested transport shape, not live
  compatibility with every provider or model.
- Secure RuntimeClass configuration, scheduling segregation, and NetworkPolicy
  are platform prerequisites. Aster validation cannot prove VM or sandbox
  isolation.
- Public-provider or provider-specific smoke tests are evidence for that exact
  environment only and must not become a universal compatibility claim.
- The generic executor contains only its published tools. Missing runtime tools
  are evaluation failures, not permission to expand the image during a frozen
  comparison.
- Shadow results and telemetry stay private even when the authoritative analysis
  is public.
