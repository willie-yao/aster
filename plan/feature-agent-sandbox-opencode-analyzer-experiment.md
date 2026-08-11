---
goal: Determine whether a thin OpenCode Agent Sandbox analyzer can replace the in-process analyzer with less dashboard-owned complexity
version: 1.0
date_created: 2026-08-10
last_updated: 2026-08-10
owner: prow-ai-dashboard maintainers
status: 'In progress'
tags: [feature, experiment, ai, agent-sandbox, opencode, analyzer]
---

# Agent Sandbox OpenCode analyzer experiment

![Status: In progress](https://img.shields.io/badge/status-In%20progress-yellow)

## 1. Replacement hypothesis

A direct OpenCode analyzer should inspect a pinned read-only source checkout and
a bounded read-only artifact tree with its native debugging harness, then return
one validated analysis. The dashboard should own workload lifecycle, isolation,
budgets, result validation, citation verification, cleanup, and telemetry. It
should not own another model tool loop or evidence-reasoning framework.

The in-process analyzer remains authoritative throughout this plan.

## 2. Hard boundaries

- One OpenCode session and one result per trial. Bash, delegation, and web tools are denied.
- No critic, semantic judge, digest, repair, revision, or planner request.
- No case-specific rules or answer-bearing artifact selection.
- No provider credential in the Sandbox.
- No public output, cache, issue, fix, notification, correction, remediation, or
  resolution effect.
- Artifact staging uses safe paths, deterministic ordering, file-count limits,
  per-file byte limits, total-byte limits, and hashes only.
- Source and artifacts are mounted read-only. Only temporary runtime state and
  one result directory are writable.

## 3. Simplicity gate

The replacement hypothesis fails if the prototype requires any of these:

- a dashboard-owned LLM tool loop;
- more than one model session or result;
- semantic evidence ranking or digest construction;
- a critic, judge, repair, or revision pass;
- a second private ledger format;
- runtime-specific lifecycle logic outside the shared Agent Sandbox seam.

Quantitative targets:

- at most one new analyzer adapter package and one executor command;
- at most 1,200 net new non-test Go lines specific to the analyzer contract,
  executor, and adapter, excluding shared lifecycle code, tests, and docs;
- before production replacement, projected deletion of in-process
  analyzer-specific code must be at least twice the long-lived analyzer-specific
  code added by this experiment.

Similar diagnostic quality without a credible simplification and deletion path
is not success.

## 4. Focused implementation sequence

### Phase 1: File-backed contract and executor

- Seal one canonical failure request, consumer prompt, pinned source identity,
  and deterministic artifact file manifest.
- Run OpenCode once against pre-mounted workspace directories.
- Require one strict `analysis.json` result.
- Verify source cleanliness, artifact hashes, exact citations, output bounds,
  and compatibility with `ai.FailureAnalysisResult`.
- Do not add Kubernetes or fetcher integration.

Exit gate:

- focused tests, full backend tests, vet, staticcheck, and independent exact-head
  review pass;
- no more than one OpenCode invocation exists in the execution path;
- net analyzer-specific non-test Go lines remain within the simplicity target.

### Phase 2A: Agent Sandbox lifecycle and workspace shape

- Reuse `agentsandbox.Runner` and the existing Kubernetes lifecycle adapter.
- Add one immutable stager init-container request.
- Mount `source/` and `artifacts/` read-only, `result/` read-write, and temporary
  state separately in the executor.
- Bind both requests, both images, resources, and the complete Pod shape to the
  execution identity.
- Add the private analyzer adapter and preserve validated results when cleanup is
  pending.
- Do not add Helm, credentials, image publication, or live deployment.

Exit gate:

- hostile workspace shapes are rejected;
- stage request changes alter the workload identity;
- result validation, timeout, cancellation, and UID-checked cleanup semantics
  pass locally;
- the existing Fix PR and critic workload shapes do not change.

### Phase 2B: Credential-free staging and runtime images

- Require one fixed pre-populated read-only PVC.
- Verify and copy one manifest-addressed source and artifact snapshot without
  network access or credentials in the Sandbox.
- Materialize only the pinned commit with shallow Git history.
- Build separate immutable analyzer executor and stager image targets.
- Keep Helm, image publication, and live deployment out of this phase.

Exit gate:

- changed, extra, linked, or oversized input files fail staging;
- the executor never mounts the input claim;
- source history is limited to the pinned commit;
- both runtime images build and run as UID 65532 without credentials.

### Phase 2C: Deployment and security

- Add disabled Helm references for immutable stager and analyzer executor images.
- Add disabled Helm values, narrow RBAC, admission, network policy, quotas, and
  RuntimeClass enforcement.
- Keep source and storage credentials outside the Sandbox and model credentials
  in the gateway only.
- Keep the runtime private, disabled, and non-authoritative.

Exit gate:

- source and artifacts remain unchanged during hostile-write tests;
- public egress and incorrect workload identity are denied;
- result retrieval and UID-checked cleanup succeed without leaked Sandboxes or
  Pods.

### Phase 3: Repeated cold comparison

- Give the in-process and OpenCode arms the same source revision, consumer
  prompt, failure metadata, and artifact snapshot identity.
- Run at least five cold repetitions for regression and control cases.
- Freeze at least ten unrelated blinded holdouts before inspecting results.
- Use separate caches and private ledger identities for every trial.

Report separately:

1. diagnostic quality and complete initiating-cause chains;
2. structured validity and no-result outcomes;
3. artifact and source citation grounding;
4. lifecycle, finalization, cleanup, and leaked identities;
5. latency and model-request counts;
6. input, cached-input, and output tokens;
7. cost or nano-AIU when available;
8. dashboard-owned implementation size and projected deletion ratio.

Exit gate:

- blinded diagnostic quality is not materially worse than the in-process arm;
- correct controls have no critical regressions;
- cleanup succeeds for every trial;
- invalid and no-result rates are explicitly acceptable;
- the simplicity gate still passes.

### Phase 4: Stop or replacement proposal

If any quality, lifecycle, efficiency, or simplicity gate fails, stop and retain
the in-process analyzer. If all gates pass, write a separate unimplemented
replacement proposal covering cache migration, operator visibility, rollback,
privacy, and staged authority transfer.

This plan does not authorize production activation or removal of the in-process
analyzer.
