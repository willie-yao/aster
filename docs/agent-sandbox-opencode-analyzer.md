# Agent Sandbox OpenCode analyzer

Status: private lifecycle prototype. It is not wired to Helm, the fetcher, the
worker, public output, or cache acceptance.

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
- one bounded, content-addressed stager request;
- one shared `emptyDir` workspace;
- read-only `source/` and `artifacts/` subPath mounts in the executor;
- one writable `result/` subPath mount;
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

This phase defines the workload shape and private analyzer adapter only. It does
not define source or artifact credentials, a staging implementation, Helm values,
admission policy, network policy, or a live deployment.

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

Helm wiring, the concrete stager, admission, network policy, and shadow
orchestration are intentionally deferred to the next focused change.

## Validation

```bash
cd backend
go test ./internal/agentanalysis ./internal/analysisexecutor ./cmd/analysisexecutor -count=1
go test ./... -count=1
go vet ./...
staticcheck ./...
```
