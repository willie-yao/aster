---
goal: Determine whether the private Agent Sandbox causal critic produces repeatable causal improvements over the in-process analyzer
version: 1.0
date_created: 2026-08-10
last_updated: 2026-08-10
owner: prow-ai-dashboard maintainers
status: 'In progress'
tags: [feature, experiment, ai, agent-sandbox, causal-critic, benchmark]
---

# Introduction

![Status: In progress](https://img.shields.io/badge/status-In%20progress-yellow)

This plan defines the private experimental program for measuring whether an independent Agent Sandbox critic detects and helps repair causal gaps that remain after the authoritative in-process analyzer and same-model semantic judge. The critic remains disabled by default, private, sampled, and non-authoritative. No phase may change public dashboard output, cache acceptance, issues, fixes, notifications, remediation, corrections, or resolution state.

## 1. Requirements & Constraints

- **REQ-001**: Compare the independent critic against the exact authoritative in-process draft and exact frozen evidence identity used for each trial.
- **REQ-002**: Report lifecycle success, structured validity, evidence grounding, causal quality, cleanup, latency, requests, tokens, nano-AIU or cost, and invalid/no-result trials as separate dimensions.
- **REQ-003**: Preserve first-attempt failure telemetry when a bounded repair attempt is introduced. A repaired result must not replace or hide the original invalid result.
- **REQ-004**: Use repeated cold trials with exact model, executor image, Sandbox workload, source, evidence, prompt, and runtime identities.
- **REQ-005**: Treat Flatcar, Kueue fixture/oracle, GCP, and Secrets as regression or authoring cases. Add separate blinded holdouts before drawing promotion conclusions.
- **SEC-001**: Keep provider credentials exclusively in the gateway. Critic Sandboxes remain tokenless, digest-pinned, read-only, and isolated by admission and network policy.
- **SEC-002**: Persist every ledger, preflight, benchmark, and diagnostic artifact outside public dashboard storage.
- **SEC-003**: Retain exact Sandbox UID cleanup identity until cleanup is confirmed. Never evict unresolved cleanup records under count or byte pressure.
- **CON-001**: The in-process analyzer remains authoritative for every phase.
- **CON-002**: Do not add case-specific rules, failure recipes, broader artifact retrieval, or model-directed evidence planning before the deterministic digest experiment passes its gates.
- **CON-003**: Do not select or report only the best trial. Report distributions and all invalid, timeout, unavailable, and cleanup-pending outcomes.
- **CON-004**: Do not merge or deploy an experimental follow-up without separate authorization.
- **GUD-001**: Prefer deterministic dashboard-owned selection, hashing, bounds, validation, and accounting. Models may propose findings or revisions but may not enforce policy.
- **PAT-001**: Extend the private `backend/internal/causalcritic` ledger and benchmark contracts rather than introducing a second persistence format.

## 2. Implementation Steps

### Implementation Phase 1: Deterministic diagnostics and durable preflight accounting

- GOAL-001: Make every invalid critic result and every candidate-local pre-model failure measurable after runtime identity is available, while preventing failed or duplicate candidates from repeatedly consuming artifact I/O.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | Create this implementation plan on a branch stacked from PR #376 head `c2eab5f46446c07f9e2818df20a9381812166cae`. | ✅ | 2026-08-10 |
| TASK-002 | Add stable validation reason codes in `backend/internal/causalcritic/contract.go` and `execution.go`. Preserve `errors.Is` behavior for `ErrInvalidInput`, `ErrInvalidReview`, `ErrMalformedResult`, and `ErrResultContract`. | ✅ | 2026-08-10 |
| TASK-003 | Add a bounded `failure_code` field to executor results and private trial records. Record deterministic validation codes separately from coarse lifecycle status. | ✅ | 2026-08-10 |
| TASK-004 | Add bounded preflight identities and claim/finalize records to `backend/internal/causalcritic/ledger.go`. The identity must include candidate request hash, authoritative hash, source revision, skill hash, critic contract version, and full runtime identity. | ✅ | 2026-08-10 |
| TASK-005 | Integrate preflight claim/finalization into `backend/internal/fetcher/causal_critic.go` before uncached evidence retrieval. Evidence failures receive a short retry retention; deterministic paired-input failures receive normal attempt retention. | ✅ | 2026-08-10 |
| TASK-006 | Extend `backend/internal/e2e/causal_critic_benchmark_test.go` summaries with failure-code distributions and preflight counts without changing public output. | ✅ | 2026-08-10 |
| TASK-007 | Add unit tests for reason-code stability, preflight duplicate suppression, stale pending reclaim, transient evidence retry, deterministic input rejection retention, and ledger count/byte pruning. | ✅ | 2026-08-10 |

Completion criteria:

- Every deterministic review rejection in the benchmark has a non-empty stable reason code.
- A previously attempted successful candidate is rejected by the durable preflight lookup without artifact I/O.
- A deterministic input failure is not retried on the next pass.
- A transient evidence failure becomes retryable after its explicit short retention.
- Existing lifecycle, UID cleanup, private-path, and public-output isolation tests remain green.
- Ledger byte accounting uses the same indented, newline-terminated encoding written to disk, so a persisted ledger cannot pass the write bound and fail the subsequent read bound.

### Phase 1 delivered contract

- Ledger schema `3` stores bounded preflight tombstones separately from compact trial-attempt tombstones and detailed records. Schema `2` private ledgers migrate in memory and are rewritten as schema `3` on the next update.
- Preflight identity includes request hash, authoritative draft hash, source revision, skill-set hash, critic contract version, and full Sandbox runtime identity.
- Retention is one hour for abandoned pending work, six hours for evidence failures, and 30 days for deterministic input failures and submitted trials.
- `error_code` remains the coarse lifecycle category. `failure_code` is the stable detailed executor, validation, or preflight category.
- Benchmark summaries report lifecycle statuses, coarse error codes, detailed failure codes, preflight statuses, and preflight failure codes independently.
- This phase changes no public output and adds no authority, fallback, cache acceptance, issue, fix, notification, correction, remediation, or resolution path.

### Implementation Phase 2: Critic-specific deterministic evidence digest

- GOAL-002: Reduce critic input while preserving the evidence needed to detect missing initiating causes.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-008 | Add `backend/internal/causalcritic/digest.go` with a deterministic 8 KiB target and 16 KiB hard cap. |  |  |
| TASK-009 | Include exact authoritative citations, source-line context, bounded causal timeline events, high-specificity errors, later-success counterevidence, ownership signals, and evidence omission metadata. |  |  |
| TASK-010 | Add digest schema, digest hash, selected-line provenance, input byte count, and omission counts to the private trial record. |  |  |
| TASK-011 | Add benchmark arms for current full bundle and deterministic digest using the same authoritative draft, model route, and runtime identity. |  |  |
| TASK-012 | Add fixtures proving digest stability under excerpt ordering and proving exact citation preservation under repeated log messages. |  |  |

Completion criteria:

- Median critic input is at most 8,000 tokens and P95 is at most 12,000 tokens on the regression matrix.
- Digest selection is byte-for-byte deterministic for an identical frozen bundle.
- Complete-diagnosis hits do not regress relative to the current full-bundle arm.

### Implementation Phase 3: One bounded structural repair attempt

- GOAL-003: Improve JSON and contract validity without hiding first-attempt failures or selecting the best of multiple results.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-013 | Add one repair request only for parse, schema, identity-copy, verdict/findings, and evidence-reference reason codes. |  |  |
| TASK-014 | Send only the invalid output, reason code, exact contract, and allowed evidence references to the repair request. Do not add evidence or causal hints. |  |  |
| TASK-015 | Persist first-attempt and repair request counts, tokens, latency, failure codes, and final validity separately. |  |  |
| TASK-016 | Reject repaired findings that change pair identity, cite new evidence, or exceed existing bounds. |  |  |

Completion criteria:

- At most two total model requests per critic trial.
- Overall structured validity is at least 95% and correct-control validity is 100%.
- Reports expose the original invalid rate and the repair success rate separately.

### Implementation Phase 4: Repeated cold causal-quality matrix

- GOAL-004: Determine whether independent objections add causal information beyond the in-process analyzer and same-model judge.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-017 | Run at least five cold repetitions for each regression case and each arm with separate caches. |  |  |
| TASK-018 | Add at least ten blinded holdout cases across unrelated infrastructure, controller, storage, networking, authentication, and test-harness failures. |  |  |
| TASK-019 | Score complete initiating-cause chains, supported causal links, false objections, evidence grounding, and control passes separately. |  |  |
| TASK-020 | Compare in-process authoritative, same-model judge, full-bundle critic, and digest critic distributions. |  |  |
| TASK-021 | Publish a private machine-readable summary containing all trials, invalid results, no-results, latency, requests, tokens, cost, nano-AIU, and cleanup status. |  |  |

Completion criteria:

- Complete-diagnosis hits improve by at least 15 percentage points on blinded cases.
- Already-correct authoritative analyses regress by no more than 5%.
- Correct controls have no critical false objections.
- Cleanup succeeds in 100% of trials with no leaked Sandbox or Pod identities.
- Median added latency is at most 25 seconds and P95 is at most 60 seconds.

### Implementation Phase 5: Offline revision utility

- GOAL-005: Measure whether bounded critic findings can improve a draft without allowing the critic to publish or replace the authoritative analyzer.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-022 | Add an offline-only revision arm that receives the authoritative draft, validated critic findings, and the original frozen evidence identity. |  |  |
| TASK-023 | Apply deterministic monotonic checks that preserve every already-supported cause and citation from the authoritative draft. |  |  |
| TASK-024 | Score critic objection quality and revised-diagnosis quality as separate outcomes. |  |  |
| TASK-025 | Keep revision output out of caches, dashboard JSON, issues, fixes, notifications, corrections, remediation, and resolution. |  |  |

Completion criteria:

- Revision improves complete-diagnosis hits beyond objection-only scoring.
- No revision loses an already-supported initiating cause or citation.
- No public or authoritative state changes during the experiment.

### Implementation Phase 6: Promotion or stop decision

- GOAL-006: Produce an evidence-backed decision without activating the critic.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-026 | Produce a private decision report comparing all validity, causal-quality, regression, latency, cost, and cleanup gates. |  |  |
| TASK-027 | Stop the experiment if two post-digest cold matrices fail to show material causal improvement, controls remain unstable, Kueue oracle remains unreliable, or efficiency exceeds the defined bounds. |  |  |
| TASK-028 | If all gates pass, write a separate activation design covering operator visibility, sampling policy, fail-open behavior, publication authority, rollback, and privacy review. |  |  |

Completion criteria:

- The result is either a documented stop decision or a separate, unimplemented activation proposal.
- This plan does not itself authorize production activation.

## 3. Experimental execution sequence

Each phase is a separate focused change and evidence checkpoint. Do not combine the digest, repair, benchmark expansion, and revision utility into one pull request.

| Step | Arm or activity | Input | Model requests | Output authority | Entry gate | Exit evidence |
|------|-----------------|-------|----------------|------------------|------------|---------------|
| 1 | Full-bundle critic baseline | Current paired input from PR #376 | One | None | Phase 1 diagnostics available | Failure-code and lifecycle distributions are complete |
| 2 | Deterministic digest A/B | Full bundle versus 8 to 16 KiB digest | One per arm | None | Phase 1 merged and ledger reset policy recorded | Digest is deterministic and does not regress complete diagnoses |
| 3 | Structural repair A/B | Invalid first output plus contract-only repair input | At most two | None | Digest arm selected and invalid reasons measured | First-attempt and repaired validity are reported separately |
| 4 | Repeated cold matrix | Authoritative, same-model judge, full critic, digest critic, optional repair | Arm-specific | None | Frozen manifests and blinded holdouts exist | Distribution report meets or fails the quality, cost, latency, and cleanup gates |
| 5 | Offline revision utility | Authoritative draft, validated critic findings, original evidence identity | One additional offline request | None | Critic objections show material precision and recall | Revision utility improves quality without monotonic regressions |
| 6 | Promotion or stop report | Private aggregate evidence only | None | None | Two complete post-digest matrices | Stop decision or separate unimplemented activation design |

### Trial identity and cold-run rules

A trial is independent only when its authoritative cache, critic ledger identity, Sandbox execution identity, and benchmark result row are new for that repetition. The report must retain exact engine commit, source revision, evidence hash, draft hash, pair hash, skill hash, executor image digest, Sandbox workload identity, gateway route, provider-reported model, and repetition number. Warm-cache reuse, duplicate suppression, parser-only fixtures, or recovered ledger rows do not count as cold quality evidence.

### Measurement contract

Report these dimensions separately for every arm and case:

1. Lifecycle: submitted, terminal, result available, validation checked, cleanup complete, finalized.
2. Validity: parse success, schema success, identity success, evidence-reference success, and final structured validity.
3. Causal quality: initiating cause, intermediate links, downstream symptoms, ownership boundaries, success counterevidence, unsupported claims, and false objections.
4. Efficiency: requests, input tokens, cached input tokens, output tokens, nano-AIU or cost, runtime duration, cleanup duration, and total added latency.
5. Missing outcomes: timeout, cancellation, unavailable runtime, no result, malformed result, contract violation, cleanup pending, and pruned detail with retained tombstone.

Aggregate reports must include counts and distributions. Do not collapse invalid or missing trials into a success denominator and do not select the best repetition.

### Holdout and scoring protocol

- Use Flatcar, Kueue fixture/oracle, GCP, and Secrets only for authoring and regression checks.
- Freeze at least ten unrelated blinded cases before inspecting critic output.
- Include already-correct controls and cases with incomplete evidence, not only known misses.
- Score critic objection quality separately from any later revised diagnosis.
- Record scorer limitations and unresolved disagreements. A lexical signal hit is not sufficient evidence of a correct causal chain.

### Stop conditions

Stop before model-directed evidence planning or production activation if any of the following remains true after two post-digest cold matrices:

- No material improvement on blinded complete-diagnosis results.
- Critical false objections occur on correct controls.
- Digest arms lose initiating-cause evidence relative to the full bundle.
- Invalid, timeout, unavailable, or cleanup-pending outcomes exceed the Phase 4 gates.
- Cost or latency exceeds the defined bounds without a compensating quality gain.
- Results depend on a named regression case, answer-bearing rule, or case-specific recipe.

## 4. Alternatives

- **ALT-001**: Expand artifact retrieval before measuring critic validity. Rejected because current Kueue oracle evidence is already present and the observed limitation is causal synthesis, not simple retrieval absence.
- **ALT-002**: Add Kueue-specific rules or recipes. Rejected because one named case is insufficient evidence for a general engine rule and would contaminate later evaluation.
- **ALT-003**: Let the critic directly rewrite or publish diagnoses. Rejected because the independent critic has not demonstrated stable validity or causal improvement.
- **ALT-004**: Report only repaired or best-scoring trials. Rejected because it hides invalid-output and cost distributions.
- **ALT-005**: Use an in-memory preflight cursor. Rejected because CronJob processes restart and require durable cross-run accounting.

## 5. Dependencies

- **DEP-001**: PR #376 and its Agent Sandbox critic contracts, private ledger, benchmark harness, Helm isolation, and immutable executor image.
- **DEP-002**: Consumer-operated internal HTTPS model gateway with provider-reported model and usage telemetry.
- **DEP-003**: Agent Sandbox v0.5.3 or a subsequently revalidated compatible release.
- **DEP-004**: Private benchmark storage outside `frontend/public` and public dashboard persistence.
- **DEP-005**: Frozen authoritative benchmark JSONL for each cold trial arm.

## 6. Files

- **FILE-001**: `backend/internal/causalcritic/contract.go` for validation codes and critic input/review contracts.
- **FILE-002**: `backend/internal/causalcritic/execution.go` for executor result failure codes and runtime identity.
- **FILE-003**: `backend/internal/causalcritic/ledger.go` for preflight claims, attempts, retention, and private telemetry.
- **FILE-004**: `backend/internal/causalcritic/digest.go` for the later deterministic evidence digest.
- **FILE-005**: `backend/internal/criticexecutor/executor.go` for stable failure-code emission and bounded repair.
- **FILE-006**: `backend/internal/fetcher/causal_critic.go` for preflight integration and sampled execution.
- **FILE-007**: `backend/internal/e2e/causal_critic_benchmark_test.go` for arm execution, scoring, and reports.
- **FILE-008**: `docs/agent-sandbox-causal-critic.md` for experimental procedure and stop gates.
- **FILE-009**: `plan/feature-agent-sandbox-critic-experiment-1.md` for this executable roadmap.

## 7. Testing

- **TEST-001**: `go test ./internal/causalcritic -count=1` validates reason codes, preflight claims, retention, tombstones, cleanup preservation, and digest contracts.
- **TEST-002**: `go test -race ./internal/causalcritic ./internal/fetcher -count=1` validates concurrent ledger claim and orchestration behavior.
- **TEST-003**: `go test ./internal/criticexecutor -count=1` validates stable executor failure codes, output bounds, and repair limits.
- **TEST-004**: `go test ./internal/e2e -run 'TestAgentSandboxCausalCritic' -count=1` validates benchmark resume, report distributions, and arm metadata.
- **TEST-005**: `go test ./... -count=1`, `go vet ./...`, and `staticcheck ./...` validate the complete backend.
- **TEST-006**: `make helm-check` validates disabled defaults and security configuration after any Helm-facing change.
- **TEST-007**: Repeated cold Agent Sandbox trials validate runtime behavior; parser-only or mocked tests do not satisfy causal-quality gates.

## 8. Risks & Assumptions

- **RISK-001**: Validation repair can inflate apparent validity. Mitigation: retain and report the original invalid result and all repair telemetry.
- **RISK-002**: A compact digest can remove the initiating signal. Mitigation: A/B against the full bundle and require no complete-diagnosis regression.
- **RISK-003**: Known regression cases can leak into prompt tuning. Mitigation: keep them as authoring/regression cases and use separate blinded holdouts.
- **RISK-004**: Durable preflight suppression can hide transient storage failures. Mitigation: use a shorter explicit retention for evidence-unavailable outcomes than for deterministic input failures or completed attempts.
- **RISK-005**: Same-provider models can remain correlated despite a separate runtime. Mitigation: record exact provider/model identity and add a cross-family arm when available.
- **ASSUMPTION-001**: Completed build artifacts and source revisions are immutable enough for a stable preflight identity.
- **ASSUMPTION-002**: The gateway continues to report model and token usage honestly; absent cost remains explicitly unavailable.
- **ASSUMPTION-003**: Existing regression cases remain suitable for repeatability checks but not for blind promotion claims.

## 9. Related Specifications / Further Reading

- `docs/agent-sandbox-causal-critic.md`
- `docs/agentic.md`
- `docs/skills.md`
- `backend/internal/e2e/causal_critic_benchmark_test.go`
- Pull request #376: Agent Sandbox causal critic runtime and private benchmark foundation
