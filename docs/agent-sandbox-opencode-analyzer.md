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

Artifact bounding is mechanical. Paths are safe and sorted, each file is at
most 8 MiB, the snapshot contains at most 512 files, and total bytes are at most
32 MiB. Every artifact path, size, and SHA-256 digest is sealed in the request.
There is no semantic evidence ranking or excerpt selection.

The Agent Sandbox deployment phase must mount both trees read-only. OpenCode may
write only runtime state under temporary storage and exactly one result file at
`result/analysis.json`.

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
