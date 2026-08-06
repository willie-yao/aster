---
goal: Add optional Orka-backed onboarding prompt authoring
version: 1.0
date_created: 2026-08-06
last_updated: 2026-08-06
owner: prow-ai-dashboard maintainers
status: 'In progress'
tags: [feature, orka, onboarding, prompt-authoring]
---

# Introduction

![Status: In progress](https://img.shields.io/badge/status-In%20progress-yellow)

Add an explicit onboarding prompt runtime selector. Local sandboxed OpenCode remains the default. Orka is opt-in and uses an operator-owned Agent for provider credentials, model selection, network policy, and optional read-only private-repository cloning.

## 1. Requirements & Constraints

- **REQ-001**: Preserve local `srt` prompt authoring as the default.
- **REQ-002**: Add `opencode` and `orka` prompt runtime choices.
- **REQ-003**: Require Orka API and Agent reference for Orka mode.
- **REQ-004**: Permit an optional read-only Orka git Secret only in Orka prompt mode.
- **REQ-005**: Transfer the bundled prompt-generation skill through `runtime.AgentRuntime`.
- **REQ-006**: Keep Bash disabled and accept only `prompts/system.md`.
- **REQ-007**: Preserve TODO-template and handoff fallback on runtime failure.
- **REQ-008**: Preserve a valid prompt when only Orka cleanup is pending and surface a warning.
- **SEC-001**: Never send local ambient auth, model endpoint, model token, headers, or network-domain policy to Orka.
- **SEC-002**: Never serialize credential values into reviewed plans or generated files.
- **SEC-003**: Pin source repositories to immutable commit SHAs.
- **CON-001**: Do not deploy or modify a live Orka installation.
- **CON-002**: Do not change failure-analysis or source-investigation policy.

## 2. Implementation Steps

### Implementation Phase 1

- GOAL-001: Add runtime selection and promptauthor provider ownership.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | Add onboarding option and CLI fields for prompt runtime, Orka API, namespace, Agent reference, and git Secret. | ✅ | 2026-08-06 |
| TASK-002 | Validate local and Orka option ownership and credential separation. | ✅ | 2026-08-06 |
| TASK-003 | Let `promptauthor.OpenCodeRuntime` omit local provider policy for Agent-owned backends and label results by backend. | ✅ | 2026-08-06 |
| TASK-004 | Preserve validated output on cleanup-only errors and expose a cleanup warning. | ✅ | 2026-08-06 |

### Implementation Phase 2

- GOAL-002: Wire onboarding planning, diagnostics, and wizard behavior.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-005 | Construct the Orka AgentRuntime lazily so constructor failures enter the existing safe fallback. | ✅ | 2026-08-06 |
| TASK-006 | Generate request-scoped Orka execution identities and keep source refs immutable. | ✅ | 2026-08-06 |
| TASK-007 | Record runtime plus local model or Orka Agent reference in credential-free plans. | ✅ | 2026-08-06 |
| TASK-008 | Add interactive runtime selection and required Orka coordinates. | ✅ | 2026-08-06 |

### Implementation Phase 3

- GOAL-003: Document, validate, review, and merge.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-009 | Update onboarding, prompt-writing, Orka, configuration, and troubleshooting docs. | ✅ | 2026-08-06 |
| TASK-010 | Add unit and regression tests for selection, fallback, cleanup warnings, and provider separation. | ✅ | 2026-08-06 |
| TASK-011 | Run backend, frontend, Helm, race, static analysis, and diff validation. | ✅ | 2026-08-06 |
| TASK-012 | Open, review, merge, and clean up the focused PR. | | |

## 3. Alternatives

- **ALT-001**: Add a prompt-specific AgentRuntime. Rejected because the generic runtime already supports dependency injection.
- **ALT-002**: Reuse local ambient OpenCode auth in Orka. Rejected because host credentials do not cross the cluster boundary.
- **ALT-003**: Fall back from Orka to local execution. Rejected because runtime choice must remain explicit.

## 4. Dependencies

- **DEP-001**: Merged PR #320 generic Orka agent generation.
- **DEP-002**: Existing onboarding prompt validation and handoff fallback.
- **DEP-003**: Operator-owned Orka Agent and result API.

## 5. Files

- **FILE-001**: `backend/internal/onboard/options.go`
- **FILE-002**: `backend/internal/onboard/onboard.go`
- **FILE-003**: `backend/internal/onboard/prompt_agent.go`
- **FILE-004**: `backend/internal/onboard/prompt_runtime.go`
- **FILE-005**: `backend/internal/onboard/promptauthor/runtime.go`
- **FILE-006**: `backend/internal/onboard/promptdiagnostics.go`
- **FILE-007**: `backend/internal/onboard/wizard.go`
- **FILE-008**: `backend/cmd/fetcher/main.go`
- **FILE-009**: onboarding and Orka documentation

## 6. Testing

- **TEST-001**: Local default retains native model, ambient auth, and reviewed network domains.
- **TEST-002**: Orka mode omits every local provider field and transfers the skill.
- **TEST-003**: Missing Orka coordinates fall back safely without file writes.
- **TEST-004**: Cleanup-only error preserves a valid prompt and reports a warning.
- **TEST-005**: Credential values do not enter plans or rendered files.
- **TEST-006**: Full repository validation passes.

## 7. Risks & Assumptions

- **RISK-001**: Orka Agent provider configuration may be incompatible with the requested benchmark model. Probe it in the disposable benchmark phase.
- **RISK-002**: Private repository cloning depends on a correctly scoped read-only Secret.
- **ASSUMPTION-001**: The Orka Agent owns model and network policy.

## 8. Related Specifications / Further Reading

- `plan/refactor-orka-agent-generation-1.md`
- `backend/internal/orka/agentruntime.go`
- `docs/onboarding-a-new-project.md`
- `docs/orka.md`
