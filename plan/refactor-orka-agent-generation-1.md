---
goal: Generalize Orka agent generation and enable safe engine-owned skill transfer
version: 1.0
date_created: 2026-08-06
last_updated: 2026-08-06
owner: prow-ai-dashboard maintainers
status: 'In progress'
tags: [refactor, orka, agent-runtime, security, onboarding]
---

# Introduction

![Status: In progress](https://img.shields.io/badge/status-In%20progress-yellow)

This plan generalizes the Orka-backed `runtime.AgentRuntime` without weakening the existing fix-generation contract. It adds deterministic engine-owned skill transfer, explicit task-purpose metadata, fail-closed execution-policy validation, exact resource cleanup, and the compatibility seam required for a later onboarding prompt-authoring PR.

## 1. Requirements & Constraints

- **REQ-001**: Preserve `runtime.AgentRuntime` as the shared local and Orka generation interface.
- **REQ-002**: Preserve immutable lowercase 40-character repository revisions for every Orka Task.
- **REQ-003**: Preserve structured result validation, local diff reconstruction, changed-file validation, and generation-only push rejection.
- **REQ-004**: Transfer every `GenerateSpec.Skills` entry into a deterministic trusted prompt preamble in name order because Orka agent Tasks do not support task-level skill overrides.
- **REQ-005**: Include task purpose, skill names and content, Agent reference, namespace, runtime version, retries, Bash policy, turn limit, timeout, git-secret policy, instruction, repository identity, and execution ID in Task identity.
- **REQ-006**: Preserve current fix-generation labels, annotations, priority, and Task name prefix by default.
- **REQ-007**: Support a distinct prompt-authoring task purpose without classifying it as fix generation.
- **REQ-008**: Preserve a valid generation result when exact resource cleanup is the only error and expose that condition as `runtime.ErrCleanupPending`.
- **SEC-001**: Reject local-provider configuration that an Orka Agent cannot enforce, including model, native model, ambient auth, endpoint, token, extra headers, and network-domain policy.
- **SEC-002**: Never place repository tokens, model credentials, result API tokens, or credential values in Task objects, Task names, logs, tests, or errors.
- **SEC-003**: Validate skill names and nonempty bounded skill contents before any cluster write.
- **SEC-004**: Keep engine-owned skill contents inside the Task prompt and never create auxiliary Kubernetes resources for skill transfer.
- **SEC-005**: Keep exact-UID cleanup limited to the generated Orka Task.
- **SEC-006**: Do not mutate the operator-owned Agent or its configured Skill references.
- **SEC-007**: Preserve the current fix admission contract and keep fix callers from supplying a git Secret or unsupported AI fields.
- **CON-001**: Do not add Kubernetes SIGs Agent Sandbox or change failure-analysis runtime policy.
- **CON-002**: Do not deploy to production or modify a live Orka release.
- **CON-003**: Do not add backward-compatibility branches unless a current caller requires them.
- **CON-004**: Use a dedicated worktree based on exact `origin/main` and do not rebase.
- **GUD-001**: Use short factual comments and idiomatic Go matching surrounding code.
- **GUD-002**: Keep PR 1 focused on the generic runtime contract. Onboarding CLI selection belongs to PR 2.
- **PAT-001**: Use Kubernetes server-side apply for deterministic Task materialization.
- **PAT-002**: Use UID and resource-version preconditions for destructive cleanup.

## 2. Implementation Steps

### Implementation Phase 1

- GOAL-001: Define the generic Orka generation contract and deterministic identities.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | Add an explicit Orka generation purpose to `backend/internal/orka/agentruntime.go`, defaulting to the existing fix contract and defining prompt-authoring metadata for PR 2. | ✅ | 2026-08-06 |
| TASK-002 | Replace fix-specific internal helper names with generation-neutral helpers while preserving existing fix values. | ✅ | 2026-08-06 |
| TASK-003 | Validate unsupported local-provider fields before calling any Kubernetes API. | ✅ | 2026-08-06 |
| TASK-004 | Extend Task fingerprinting to serialize purpose, git-secret policy, and sorted skill names and contents. | ✅ | 2026-08-06 |
| TASK-005 | Add unit tests proving purpose isolation, unsupported-policy rejection, and fingerprint sensitivity. | ✅ | 2026-08-06 |

### Implementation Phase 2

- GOAL-002: Transfer and clean up engine-owned skills safely.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-006 | Preserve exact Task UID and resource-version cleanup in `backend/internal/orka/agentruntime.go`. | ✅ | 2026-08-06 |
| TASK-007 | Validate skill names and contents and build a deterministic trusted prompt preamble. | ✅ | 2026-08-06 |
| TASK-008 | Prepend sorted engine-owned skill sections to the Orka Task prompt while preserving the original instruction verbatim as the final task section. | ✅ | 2026-08-06 |
| TASK-009 | Include exact skill names and contents in Task identity and keep cleanup Task-only. | ✅ | 2026-08-06 |
| TASK-010 | Add tests for skill ordering, prompt transfer, cancellation cleanup, UID mismatch protection, and cleanup-only errors. | ✅ | 2026-08-06 |

### Implementation Phase 3

- GOAL-003: Keep current fix callers and admission compatible.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-011 | Update fix-runtime construction so Orka does not receive local model, endpoint, token, header, or network policy fields. | ✅ | 2026-08-06 |
| TASK-012 | Preserve local OpenCode configuration and regression-test local versus Orka spec construction. | ✅ | 2026-08-06 |
| TASK-013 | Preserve the existing fix admission policy and verify the generated Task does not add unsupported AI or skill fields. | ✅ | 2026-08-06 |
| TASK-014 | Run the existing Helm render checks to prove fix admission compatibility. | ✅ | 2026-08-06 |
| TASK-015 | Update Orka and fix-runtime documentation with provider ownership, skill transfer, and cleanup semantics. | ✅ | 2026-08-06 |

### Implementation Phase 4

- GOAL-004: Validate, review, publish, and merge the focused PR.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-016 | Run focused Orka, runtime, fixpr, actions, fetcher, and Helm tests. | ✅ | 2026-08-06 |
| TASK-017 | Run changed-package race tests and static analyzers. | ✅ | 2026-08-06 |
| TASK-018 | Run repository-wide backend and frontend validation plus `git diff --check`. | ✅ | 2026-08-06 |
| TASK-019 | Commit without GPG signing, push, and open a ready PR. | | |
| TASK-020 | Verify exact-head CI and Copilot feedback, address only actionable current-head findings, merge, and clean up. | | |

## 3. Alternatives

- **ALT-001**: Create task-level skill references. Rejected because current Orka exposes skills on AI Tasks and Agent defaults, not as overrides on type agent Tasks.
- **ALT-002**: Require operators to pre-create a fixed Orka Skill CR. Rejected because it can drift from the engine-embedded skill and requires mutation of the referenced Agent to consume it.
- **ALT-003**: Clone or mutate the referenced Agent to add temporary skills. Rejected because it races with operator ownership and expands credential-bearing resource writes.
- **ALT-004**: Add a prompt-specific AgentRuntime implementation. Rejected because `runtime.AgentRuntime` is intentionally generic and prompt authoring already supports dependency injection.
- **ALT-005**: Continue silently ignoring provider and network fields in Orka. Rejected because the effective execution policy would differ from the caller-visible request.

## 4. Dependencies

- **DEP-001**: `runtime.GenerateSpec` in `backend/internal/runtime/agent.go`.
- **DEP-002**: Orka `core.orka.ai/v1alpha1` Agent Task schema and OpenCode harness behavior validated against upstream commit `21f8ef15923e505eb89252be9a043f57fb2f2ef0`.
- **DEP-003**: Existing Orka Task server-side apply and result API contracts.
- **DEP-004**: Existing Orka result API and structured generation result version 1.
- **DEP-005**: Existing fix-runtime admission values in the Helm chart.

## 5. Files

- **FILE-001**: `backend/internal/orka/agentruntime.go`, generic purpose, skill transfer, policy validation, identity, and cleanup.
- **FILE-002**: `backend/internal/orka/agentruntime_test.go`, runtime contract and cleanup regression tests.
- **FILE-003**: `backend/internal/orka/kube.go`, unchanged exact Task identity helpers.
- **FILE-004**: `backend/internal/orka/agentruntime_test.go`, skill prompt and Task identity tests.
- **FILE-005**: `backend/internal/runtime/agent.go`, backend-neutral skill and cleanup-result documentation if required.
- **FILE-006**: `backend/internal/fixpr/agent_gen.go` and `backend/internal/fixpr/build_failure.go`, backend-owned provider policy handling if required.
- **FILE-007**: `backend/internal/actions/actions.go`, Orka fix Agent configuration.
- **FILE-008**: `backend/internal/fetcher/fetcher.go`, scheduled Orka fix Agent configuration.
- **FILE-009**: `deploy/helm/prow-ai-dashboard/templates/orka-fix-runtime-admission.yaml`, fix skill prohibition.
- **FILE-010**: `deploy/helm/prow-ai-dashboard/test-render.sh`, admission regression assertions.
- **FILE-011**: `docs/orka.md`, `docs/orka-architecture.md`, `docs/fix-prs.md`, and `docs/project-configuration.md`, contract documentation.
- **FILE-012**: `plan/refactor-orka-agent-generation-1.md`, executable implementation plan and completion record.

## 6. Testing

- **TEST-001**: Orka purpose defaults preserve existing fix Task names and metadata.
- **TEST-002**: Prompt purpose produces distinct names and metadata.
- **TEST-003**: Every unsupported local-provider field fails before Kubernetes calls.
- **TEST-004**: Skill map ordering does not change Task identity.
- **TEST-005**: Skill content changes do change Task identity.
- **TEST-006**: Task prompt contains sorted trusted skill sections and no credential fields.
- **TEST-007**: Task apply failure attempts exact Task cleanup when the execution identity is known.
- **TEST-008**: Task cleanup failure returns `runtime.ErrCleanupPending`.
- **TEST-009**: Successful Task cleanup returns the validated generation result.
- **TEST-010**: Successful generation plus cleanup-only failure preserves the generated files and diff.
- **TEST-011**: Existing Orka fix configuration omits local model endpoint policy while local OpenCode preserves it.
- **TEST-012**: Existing Helm admission render and operation checks remain clean.
- **TEST-013**: `cd backend && go test ./... -count=1` passes.
- **TEST-014**: `cd backend && go build ./...` and `go vet ./...` pass.
- **TEST-015**: `cd backend && staticcheck ./...` passes or reports only verified pre-existing findings.
- **TEST-016**: Relevant race tests and changed-package golangci-lint pass.
- **TEST-017**: `make check-repo-map` and `make helm-check` pass.
- **TEST-018**: Frontend install, typecheck, test, lint, and build pass.
- **TEST-019**: `git diff --check` passes.

## 7. Risks & Assumptions

- **RISK-001**: Upstream Orka currently has no published release or tag. Benchmarking must pin exact commit `21f8ef15923e505eb89252be9a043f57fb2f2ef0` and report that limitation.
- **RISK-002**: Inlining skills increases Task prompt size. Skill size and aggregate bounds must reject oversized content before any cluster write.
- **RISK-003**: Tightening provider-field validation can break callers that currently rely on silent omission. All current call sites must be audited and tested in the same PR.
- **RISK-004**: Admission expressions can unintentionally widen or block live fix Tasks. Render tests must assert the complete fixed contract.
- **ASSUMPTION-001**: Orka's referenced Agent owns model endpoint, credentials, runtime image, and outbound access policy.
- **ASSUMPTION-002**: The prompt-authoring PR will configure a distinct task purpose and may optionally provide a read-only git Secret.
- **ASSUMPTION-003**: Fix generation continues to use the existing public immutable repository workspace unless separately configured in a later scoped change.

## 8. Related Specifications / Further Reading

- `backend/internal/runtime/agent.go`
- `backend/internal/orka/agentruntime.go`
- `backend/internal/orka/source_investigation.go`
- `docs/orka.md`
- `docs/orka-architecture.md`
- Orka upstream `api/v1alpha1/task_types.go` at commit `21f8ef15923e505eb89252be9a043f57fb2f2ef0`
