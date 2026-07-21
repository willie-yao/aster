---
goal: Implement a Hermetic Email Remediation Loop End-to-End Test
version: 1.1
date_created: 2026-07-21
last_updated: 2026-07-21
owner: prow-ai-dashboard maintainers
status: 'Completed'
tags: [feature, test, e2e, email, remediation, prow]
---

# Introduction

![Status: Completed](https://img.shields.io/badge/status-Completed-brightgreen)

Implement a deterministic end-to-end test that exercises the scheduled recurring-pattern email, fix-PR tracking, Prow presubmit verification, post-merge periodic verification, lifecycle emails, deduplication, restart persistence, and same-cause recurrence without requiring a real Prow failure, GitHub write, SMTP relay, AI endpoint, or external network access.

The primary scenario will run in `backend/internal/fetcher` so it can exercise `pipeline.runSideEffects` and the existing package-level test seams without exporting new production APIs. It will use the real local storage backend and real Prow artifact parsers, a fake GitHub HTTP transport, a deterministic fake fix agent and PR client, and an in-memory `notify.Sender`. A second bridge test will invoke `orka.FinalizePatternsAndRun` and prove finalized Orka pattern output reaches the same scheduled email side effects without duplicating the full lifecycle scenario.

## 1. Requirements & Constraints

- **REQ-001**: Add one hermetic sequential test named `TestEmailRemediationLoopE2E` in `backend/internal/fetcher/email_loop_e2e_test.go` using package `fetcher`.
- **REQ-002**: The test must run with no external network, real SMTP server, real GitHub repository, real Prow deployment, or real AI model.
- **REQ-003**: The scenario must preserve one temporary output directory across all steps so `notification_state.json`, `fix_pr_state.json`, `remediation_state.json`, `remediation_prow_catalog.json`, and public projections are exercised as persisted state.
- **REQ-004**: Reconstruct the `pipeline` before each scenario step to simulate process restarts while preserving the output and local artifact directories.
- **REQ-005**: Exercise this ordered lifecycle: initial systemic recurring failure, draft fix PR creation, awaiting presubmit, current-head presubmit pass, merge observation, required clean periodic runs, verified fix, and newer same-cause recurrence.
- **REQ-006**: Run every lifecycle step twice. The second unchanged run must produce zero additional email messages and zero additional GitHub write operations.
- **REQ-007**: Use real `notify.Notifier.ProcessFailures` behavior for the initial recurring-pattern email and real `pipeline.sendRemediationEmails` behavior for remediation transition emails.
- **REQ-008**: Use raw local Prow artifacts for historical coverage and current-head presubmit verification, including `started.json`, `finished.json`, `prowjob.json`, directory entries, and JUnit XML.
- **REQ-009**: Use `models.JobDetail` snapshots for originating periodic runs because finalized side effects consume already-finalized job data.
- **REQ-010**: Assert exact email transition ordering and message counts rather than substring-only presence.
- **REQ-011**: Assert that the initial systemic-pattern email contains authenticated dashboard links for issue and fix review when `notifications.email.action_links` is enabled.
- **REQ-012**: Assert private remediation evidence remains absent from `remediations.json` throughout the scenario.
- **REQ-013**: Assert the final recurrence state is `still_failing_same_cause` and that its lifecycle email links to the original pull request and dashboard job.
- **REQ-014**: Keep issue filing disabled in the primary scenario. Issue close/reopen behavior remains covered by focused `backend/internal/remediation/issues_test.go` tests.
- **REQ-015**: Keep action preview and confirmation HTTP behavior out of this scenario. Existing `backend/internal/server/server_test.go` and `backend/internal/actions/*_test.go` remain authoritative for authenticated preview and confirmation.
- **REQ-016**: Add `TestOrkaFinalizationTriggersEmailSideEffects` in `backend/internal/fetcher/email_loop_e2e_test.go`. It must call `orka.FinalizePatternsAndRun`, use a deterministic pattern analyzer, invoke the same fetcher side-effect path in the callback, and assert exactly one systemic-pattern email.
- **REQ-017**: The Orka bridge test must start from finalized per-test `AIAnalysis` data and existing dashboard/job files, proving the Orka finalization contract rather than calling OpenCode or a real Orka API. The existing `backend/internal/e2e/orka_pipeline_test.go` remains authoritative for producer, task API, event validation, and ingestor coverage.
- **SEC-001**: Fixture data must contain no production email addresses, tokens, repository secrets, private URLs, or copied private artifact content.
- **SEC-002**: Clear `AI_TOKEN`, `AI_ENDPOINT`, `AI_MODEL`, `ISSUE_TOKEN`, `FIX_TOKEN`, `GITHUB_TOKEN`, and `EMAIL_SMTP_PASSWORD` before constructing dependencies; set only deterministic dummy values required by the test.
- **CON-001**: Do not introduce Mailpit, Docker Compose, smtp4dev, a new test framework, or a new CI service dependency.
- **CON-002**: Do not add retry-PR generation, reservation protocols, issue-comment remote idempotency, or any behavior planned for the separate remediation retry PR.
- **CON-003**: Do not add historical migration or backward-compatibility branches for test fixtures or state.
- **CON-004**: Do not modify production behavior except for the minimal sender factory unification required to inject one sender for both scheduled and remediation lifecycle email paths.
- **GUD-001**: Match the current short comment style and avoid comments describing PR history or how the test was developed.
- **GUD-002**: Use deterministic build IDs `100` through `105`, pull request `7`, head SHA `head-1`, merge SHA `merge-1`, and source repository `example/repo`.
- **PAT-001**: Follow the local fixture patterns in `backend/internal/e2e/pipeline_test.go:17-104` and the persisted-side-effect patterns in `backend/internal/fetcher/postanalysis_test.go`.
- **PAT-002**: Reuse `remediation.NewReconciler`, `remediation.SetVerification`, `fixpr.NewManager`, `notify.NewNotifier`, and `statefile.WriteJSON` rather than reimplementing lifecycle logic in the test.

## 2. Implementation Steps

### Implementation Phase 1

- GOAL-001: Add deterministic dependency and fixture helpers without changing lifecycle behavior.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | In `backend/internal/fetcher/fetcher.go`, replace `newRemediationEmailSender` at approximately lines 1021-1023 with one package-level `newEmailSender func(notify.SMTPConfig) (notify.Sender, error)`. Use it for both the scheduled email sender construction near lines 382-390 and remediation sender construction near lines 1038-1041. Update existing sender-injection tests in `backend/internal/fetcher/postanalysis_test.go` to override and restore `newEmailSender`. Completion criterion: both email paths use the same injected sender and all existing tests pass unchanged semantically. | ✅ | 2026-07-21 |
| TASK-002 | Create `backend/internal/fetcher/email_loop_e2e_test.go` with an `emailLoopSender` that stores immutable copies of `notify.Message`, exposes message count and subjects, and optionally records send errors. Do not mark this test or any helper test parallel because it overrides package-level factories. Completion criterion: the helper records all scheduled and remediation messages through `newEmailSender`. | ✅ | 2026-07-21 |
| TASK-003 | In `backend/internal/fetcher/email_loop_e2e_test.go`, add `emailLoopGitHubTransport` implementing `http.RoundTripper`. Implement only the REST responses used by `ghpr.Client`: pull request reads, marker searches returning no pre-existing PR, and commit comparisons. Record request method and path counts. Completion criterion: unknown endpoints fail the test with the full method and path. | ✅ | 2026-07-21 |
| TASK-004 | Add deterministic `emailLoopFixAgent` and `emailLoopFixPRClient` helpers in the same test file. Override `newBatchFixRuntime` and `newBatchFixManager` so the real `fixpr.Manager.Reconcile` persists one tracked draft PR at `https://github.com/example/repo/pull/7`. Restore both globals with `t.Cleanup`. Completion criterion: `fix_pr_state.json` contains exactly one tracked fix after the first step. | ✅ | 2026-07-21 |
| TASK-005 | Add fixture writers in the same test file for local Prow paths. Implement helpers for a historical coverage presubmit and a current pull-request presubmit. Each helper must write directory index entries, `started.json`, `finished.json`, `prowjob.json`, and `artifacts/junit.xml` under a temporary local storage root. Completion criterion: `remediation.BuildCoverageCatalog` discovers the exact qualified JUnit identity from the historical build. | ✅ | 2026-07-21 |
| TASK-006 | Add `emailLoopScenario.pipeline` that builds a fresh `pipeline` using the same project config, output directory, local storage root, deterministic job catalog, fake GitHub transport, and fake dependency factories. Completion criterion: every scenario step creates a new pipeline instance without deleting any persisted state. | ✅ | 2026-07-21 |

### Implementation Phase 2

- GOAL-002: Exercise the complete scheduled email and remediation verification state machine.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-007 | Implement step `initial-failure` in `TestEmailRemediationLoopE2E`. Supply one high-confidence systemic periodic pattern and three failed periodic `BuildResult` entries with the same qualified test identity and error signature. Run `pipeline.runSideEffects`. Assert one systemic-pattern email, action links containing `/job/` with `action=create-issue` and `action=propose-fix`, one persisted fix reference, one remediation attempt for pull request 7, and one lifecycle email for `awaiting_presubmit`. | ✅ | 2026-07-21 |
| TASK-008 | Repeat `initial-failure` using a newly constructed pipeline and unchanged data. Assert no additional email, no second fix PR, no additional `fixpr.Manager.SearchOpenPR` or remediation recovery search after the tracked reference is loaded, and unchanged notification/remediation emailed indexes. | ✅ | 2026-07-21 |
| TASK-009 | Implement step `presubmit-pass`. Add the current pull request presubmit artifact with pull number 7, head SHA `head-1`, a successful result, and the exact expected JUnit case. Run side effects and assert remediation status `premerge_verified` plus exactly one new lifecycle email. Repeat the step and assert no new email. | ✅ | 2026-07-21 |
| TASK-010 | Implement step `merged-observing`. Change the fake pull request response to merged with merge SHA `merge-1`, but provide no eligible post-merge periodic builds. Run side effects and assert status `observing` plus exactly one new lifecycle email. Repeat and assert no new email. | ✅ | 2026-07-21 |
| TASK-011 | Implement step `periodic-clean`. Supply two newer passing periodic builds whose commits are accepted by the fake compare endpoint as containing `merge-1`. Run side effects and assert status `verified_fixed`, outcome `passed`, two recorded clean observations, and exactly one new lifecycle email. Repeat and assert no new email. | ✅ | 2026-07-21 |
| TASK-012 | Implement step `same-cause-recurrence`. Add build `105` with the same qualified test identity and normalized error signature as the original evidence. Run side effects and assert status `still_failing_same_cause`, one new recurrence email, the original pull request URL in the message, and no additional fix PR. Repeat and assert no new email. | ✅ | 2026-07-21 |

### Implementation Phase 3

- GOAL-003: Validate persistence, publication boundaries, and test integration.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-013 | After every scenario step, reload `remediation_state.json` and `remediations.json` from disk rather than relying only on in-memory values. Assert repository scope, transition index, emailed transition index, PR number, public status, and publication boundaries. Assert `notification_state.json`, `fix_pr_state.json`, and `remediation_prow_catalog.json` are created during the initial step. | ✅ | 2026-07-21 |
| TASK-014 | Decode `remediations.json` after presubmit, verified, and recurrence steps. Assert public status and links are present. Parse the raw JSON and enforce explicit key allowlists for the root, remediation, issue, attempt, and observation objects so any private evidence or observation field fails the test. | ✅ | 2026-07-21 |
| TASK-015 | Add one explicit expected subject sequence covering the initial pattern alert and all remediation lifecycle emails. Assert the final recorded sequence exactly matches the expected slice and contains no duplicate adjacent or repeated transition subject. | ✅ | 2026-07-21 |
| TASK-016 | Update `Makefile` target `e2e` at lines 69-71 to run the existing `./internal/e2e/...` suite and both new fetcher E2E tests. Do not change the default `make test` behavior. | ✅ | 2026-07-21 |
| TASK-017 | Update `docs/testing.md:43-60` to document that `make e2e` includes the hermetic email remediation loop, list its fake dependencies, and state that it never sends real email or writes to GitHub. | ✅ | 2026-07-21 |

### Implementation Phase 4

- GOAL-004: Prove the Orka finalization entry path reaches the shared email side effects.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-018 | In `backend/internal/fetcher/email_loop_e2e_test.go`, add a deterministic analyzer implementing `patterns.Analyzer`. Write one active dashboard job with three failed runs whose test cases already contain finalized `AIAnalysis`, plus an empty `flakiness.json`. | ✅ | 2026-07-21 |
| TASK-019 | Implement `TestOrkaFinalizationTriggersEmailSideEffects` by calling `orka.FinalizePatternsAndRun`. In the callback, load the finalized jobs and flakiness report with `loadFinalizedData`, create a fresh pipeline with fix PRs and issues disabled, and call `pipeline.runSideEffects`. | ✅ | 2026-07-21 |
| TASK-020 | Assert Orka finalization reports one recurring pattern, writes its stable pattern ID into `flakiness.json`, and sends exactly one systemic recurring-pattern email with the expected job and action links. Re-run only the callback against unchanged finalized output and assert notification state suppresses a duplicate. | ✅ | 2026-07-21 |
| TASK-021 | Keep `backend/internal/e2e/orka_pipeline_test.go` unchanged unless a helper must be shared. Do not duplicate producer, task API, event-stream, or ingestor assertions in the email-loop test. | ✅ | 2026-07-21 |

## 3. Alternatives

- **ALT-001**: Run Mailpit or smtp4dev and inspect received MIME messages through an HTTP API. Not chosen because the existing SMTP encoder and protocol already have focused tests in `backend/internal/notify/smtp_test.go`, while adding a container dependency would make the default E2E suite slower and less portable.
- **ALT-002**: Create a real intentionally failing Prow job. Not chosen because it requires test-infra configuration, external storage, credentials, long execution time, and nondeterministic scheduling.
- **ALT-003**: Replay a production CAPZ failure archive. Not chosen for the first implementation because the email lifecycle requires several controlled state transitions, while a static archive represents only one point in time.
- **ALT-004**: Seed `remediation_state.json` directly for every step. Not chosen because it would bypass fix tracking, Prow metadata validation, coverage discovery, transition calculation, and persistence behavior.
- **ALT-005**: Put the entire test under `backend/internal/e2e`. Not chosen because that package cannot access the existing unexported fetcher dependency factories without adding production-only exported test hooks. The test remains end-to-end in behavior while living in package `fetcher`.
- **ALT-006**: Browser-click the email action links and drive authenticated preview and confirmation in this scenario. Not chosen because server routing, action preview, and confirmation already have focused tests. Adding browser and auth orchestration would expand the test beyond the scheduled email and verification loop.

## 4. Dependencies

- **DEP-001**: Merged remediation verification behavior in `backend/internal/remediation`, currently present on main at commit `fbe3d71`.
- **DEP-002**: Local storage backend from `backend/internal/storage/local.go`.
- **DEP-003**: Prow build discovery and metadata parsing from `backend/internal/prowbuild`.
- **DEP-004**: Notification state and rendering from `backend/internal/notify`.
- **DEP-005**: Fetch-side test factories `newBatchFixRuntime`, `newBatchFixManager`, and the unified `newEmailSender` in `backend/internal/fetcher/fetcher.go`.
- **DEP-006**: Standard library `net/http`, `httptest`-compatible request handling, temporary directories, and JSON/XML fixture writing. No new module dependency is permitted.

## 5. Files

- **FILE-001**: `backend/internal/fetcher/email_loop_e2e_test.go`, new sequential hermetic scenario and all test-only helpers.
- **FILE-002**: `backend/internal/fetcher/fetcher.go`, unify scheduled and remediation email sender construction behind `newEmailSender`.
- **FILE-003**: `backend/internal/fetcher/postanalysis_test.go`, rename existing sender factory overrides to `newEmailSender` and retain focused delivery/reload tests.
- **FILE-004**: `Makefile`, include the new scenario in `make e2e`.
- **FILE-005**: `docs/testing.md`, document the new scenario and its hermetic boundaries.
- **FILE-006**: No committed production Prow artifact fixture files are required; `email_loop_e2e_test.go` writes the compact raw artifacts into `t.TempDir()`.
- **FILE-007**: `backend/internal/e2e/orka_pipeline_test.go` remains unchanged by default and provides complementary Orka producer and ingestor coverage.

## 6. Testing

- **TEST-001**: `cd backend && go test ./internal/fetcher -run '^TestEmailRemediationLoopE2E$' -count=1 -v` must pass and report no skipped steps.
- **TEST-002**: `cd backend && go test ./internal/fetcher -run '^TestOrkaFinalizationTriggersEmailSideEffects$' -count=1 -v` must pass.
- **TEST-003**: `make e2e` must run the existing pipeline E2E tests and both new fetcher scenarios.
- **TEST-004**: `cd backend && go test ./... -count=1` must pass.
- **TEST-005**: `cd backend && go test -race -count=1 ./internal/fetcher ./internal/remediation ./internal/notify` must pass.
- **TEST-006**: `cd backend && go vet ./...` must pass.
- **TEST-007**: `cd backend && staticcheck ./...` must pass.
- **TEST-008**: `cd frontend && npm run build && npm run lint` must pass even though the plan does not require frontend changes.
- **TEST-009**: `helm lint deploy/helm/prow-ai-dashboard` must pass.
- **TEST-010**: `git diff --check` must pass.

## 7. Risks & Assumptions

- **RISK-001**: Package-level factory overrides can leak between tests. Mitigation: do not use `t.Parallel`, capture every prior factory, and restore it with `t.Cleanup` before any assertion that can fail.
- **RISK-002**: The fake GitHub transport can drift from endpoints used by `ghpr.Client`. Mitigation: reject every unknown method/path and assert the complete recorded request set at each step.
- **RISK-003**: A single large test can be difficult to diagnose. Mitigation: implement named step helpers that call `t.Run` sequentially while sharing one explicit `emailLoopScenario` state object.
- **RISK-004**: `t.Run` subtests may execute after parent cleanup if parallelized. Mitigation: never mark the parent or subtests parallel.
- **RISK-005**: Initial pattern analysis is AI-generated in production. Assumption: this scenario starts from deterministic finalized `PatternAnalysis` data because the agentic analysis path is independently covered by scripted E2E tests.
- **RISK-006**: Issue lifecycle is disabled in this scenario. Assumption: issue close/reopen tests remain authoritative and the email loop does not require a GitHub issue to verify PR and email state transitions.
- **RISK-007**: The test uses batch fix generation rather than browser confirmation. Assumption: authenticated preview/confirm and email action-link formats remain covered by their existing server, actions, and notification tests.
- **ASSUMPTION-001**: Prow directory listings are newest-first and the local backend mirrors the same relative path contract used by remote providers.
- **ASSUMPTION-002**: One historical presubmit is sufficient to populate the periodic-to-presubmit coverage mapping for the exact test identity.
- **ASSUMPTION-003**: The merged remediation verification code on `origin/main` is the authoritative behavior; the old pre-split backup branch is not used by this test.
- **ASSUMPTION-004**: Existing `backend/internal/e2e/orka_pipeline_test.go` coverage, combined with the new `orka.FinalizePatternsAndRun` bridge test, is sufficient to cover the Orka track without running a real Orka service in the email lifecycle scenario.

## 8. Related Specifications / Further Reading

- `docs/testing.md`
- `docs/fix-prs.md#closed-loop-prow-verification`
- `docs/notifications.md#state-and-retry-behavior`
- `backend/internal/e2e/pipeline_test.go`
- `backend/internal/fetcher/postanalysis.go`
- `backend/internal/fetcher/postanalysis_test.go`
- `backend/internal/remediation/integration_test.go`
- `backend/internal/notify/smtp_test.go`
