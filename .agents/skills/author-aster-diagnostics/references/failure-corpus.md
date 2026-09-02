# Failure corpus and prompt-authoring workflow

## Contents

- [Build a representative corpus](#build-a-representative-corpus)
- [Split cases before diagnosis](#split-cases-before-diagnosis)
- [Diagnose each failure](#diagnose-each-failure)
- [Write the deterministic corpus](#write-the-deterministic-corpus)
- [Extract prompt knowledge](#extract-prompt-knowledge)
- [Validate the prompt with fresh sessions](#validate-the-prompt-with-fresh-sessions)
- [Apply the prompt quality rubric](#apply-the-prompt-quality-rubric)
- [Stop when evidence is insufficient](#stop-when-evidence-is-insufficient)

## Build a representative corpus

Use engine discovery and authorized Prow artifact indexes to enumerate candidate failures. Record whether access is public or explicitly approved, and do not expand access while building the corpus. Do not select only the first recent failures or only builds that share one terminal wrapper.

When the project has enough history, target:

- At least six diagnosed failures.
- At least two jobs, job flavors, or shards.
- At least three initiating-error classes or lifecycle phases.
- At least one same-wrapper, different-cause counterexample for every proposed causal rule.
- A nearby passing build or later-success comparison for each transient rule when one is available.

These are corpus-quality targets, not reasons to invent data. Record the achieved coverage and every missing dimension. If the project is smaller, use all available independent cases and narrow the claims accordingly.

Prefer cases that expose different boundaries such as setup, controller reconciliation, API compatibility, scheduling, node readiness, storage, test assertion, and cleanup. Treat cleanup wrappers as a separate class only when they are the initiating failure.

## Split cases before diagnosis

Assign every selected case to exactly one split before reading answer-bearing material:

1. **Authoring set:** diagnose and use to draft the prompt.
2. **Validation set:** keep out of the initial prompt draft. Use for bounded prompt revision after the first draft.
3. **Final holdout:** keep embargoed until the prompt and proposals are frozen. Prefer an exact analyzer or test identity and set `holdout_event_scope` to `single_event_identity`. When only a build identity is available, set it to `build_level_unresolved`. Record `pre_freeze_holdout_kind` as a causal hypothesis only. After reveal, list every independent causal event. Aggregate recurrence plus generalization as `mixed`; do not force unrelated failures in one build into one event.

Keep all events from one build in the same split, but keep their causal-event identities separate. Related retries, duplicate artifacts, or two JUnit entries from the same initiating event count once. Independent JUnit failures in one build remain separate post-reveal events and may make the aggregate holdout `mixed`; aggregate the holdout only after event-level classification.

Do not move a failed validation or final-holdout case into the authoring set and then count it as independent. Revisions based on validation cases are allowed, but the final holdout remains one-shot evidence. For an existing prompt, compare every candidate holdout with `baseline_provenance` and reject a matching job and build or causal-event identity. A different test name in the same build remains ineligible. Job family, flavor, wrapper, or duration alone cannot define recurrence or generalization.

## Diagnose each failure

Use already-collected artifacts. Do not execute project source, tests, or binaries.

For every authoring or revealed validation case:

1. Read `prowjob.json` for the effective job, refs, environment, commands, and timing. Prove that the Prow pod started. If no `started.json` or `build-log.txt` exists, read `podinfo.json` scheduling conditions and keep ownership at the build-cluster boundary.
2. Read the exact JUnit failure or build-level failure signal.
3. Trace `build-log.txt` backward from the terminal error to the earliest initiating error and forward to determine whether recovery occurred.
4. List the artifact tree before declaring expected evidence absent.
5. Read the component, controller, workload, node, or API artifacts that can prove or disprove the leading hypotheses.
6. Read bounded source ranges at the pinned revision only when they establish stable control flow, ownership, artifact production, or API relationships.
7. Compare a passing neighbor, later-success transition, or independent same-wrapper case when available.
8. Record competing hypotheses and the evidence that rejects or leaves each one unresolved.

Write each causal step as actor, exact operation or request, response or observed state, consequence, and evidence IDs. Successful evidence for component A can exclude a failed A operation, but it cannot assign ownership to component B. Preserve test configuration, scheduler, `VolumeBinding`, attach/detach, kubelet, runtime, operating-system handoff, proxy, and external-service intermediaries until component-specific evidence closes them. For storage failures, first locate the failed lifecycle phase, then correlate the same volume, PVC, pod, node, and time window. A PVC-not-found message observed after test cleanup does not establish why a pod was Pending before cleanup. Any missing identity dimension leaves an open handoff.

Do not stop at a timeout, generic exit status, cleanup error, or the last log line. Do not infer ownership from timing proximity or a resource name.

## Write the deterministic corpus

Write `reports/failure-corpus.json` with the exact version-2 contract in [report-schema.json](report-schema.json). Validate it together with `reports/benchmark-results.json` using `scripts/validate_reports.py`.

The common report envelope includes:

- `schema_version: 2` and `document_type: failure_corpus`.
- Exact engine and consumer identity.
- A shared `freeze_manifest` object.
- Separate deterministic, fresh-holdout, and dashboard-provider evidence planes.
- Classification records with one evidence plane each.
- Validation commands whose output path and SHA-256 are preserved.

Each case records at least:

```json
{
  "id": "stable-case-id",
  "split": "authoring",
  "pre_freeze_holdout_kind": "not_applicable",
  "holdout_event_scope": "not_applicable",
  "embargoed": false,
  "causal_event_id": "stable-causal-event-id",
  "fresh_session_id": null,
  "job_name": "periodic-example",
  "build_id": "123",
  "test_name": "Prow job execution",
  "source_revision": "<40-char-sha-or-null>",
  "source_revision_status": "resolved_from_build",
  "source_revision_provenance": "prowjob.json spec.extra_refs",
  "failure_class": "api_compatibility",
  "phase_reached": "dependency_deployment",
  "initiating_error": "bounded factual statement",
  "terminal_wrapper": "bounded factual statement",
  "causal_chain": [
    {
      "actor": "scheduler",
      "operation": "watch example.io/v1 Resource",
      "response_or_state": "API returned 404",
      "consequence": "handler synchronization failed",
      "evidence_ids": ["E1", "E2"]
    }
  ],
  "evidence": [],
  "competing_hypotheses": [],
  "passing_comparison": {
    "status": "unavailable",
    "case_id": null,
    "evidence_ids": [],
    "result": null,
    "unavailable_reason": "no passing comparison was retained"
  },
  "ownership": {
    "assigned_component": null,
    "assignment_strength": "none",
    "possible_components": [],
    "excluded_components": [],
    "positive_owner_evidence_ids": [],
    "exculpatory_evidence_ids": [],
    "open_handoffs": [],
    "unresolved": "component ownership remains open",
    "storage_identity_correlation": {
      "applicable": false,
      "same_volume": "not_applicable",
      "same_pvc": "not_applicable",
      "same_pod": "not_applicable",
      "same_node": "not_applicable",
      "same_time_window": "not_applicable",
      "evidence_ids": [],
      "limitation": null
    }
  },
  "transient": {
    "status": "unresolved",
    "same_run_evidence_ids": [],
    "cross_run_evidence_ids": [],
    "non_transient_boundary": "",
    "unresolved_reason": "recovery evidence was not retained"
  },
  "reusable_project_facts": [],
  "recurrence_signatures": [],
  "prompt_candidates": [],
  "recipe_candidate": null,
  "unresolved": []
}
```

Use `pre_freeze_holdout_kind: not_applicable` and `holdout_event_scope: not_applicable` for authoring and validation cases. Every final holdout uses `recurrence`, `generalization`, or `unresolved` as a pre-freeze causal hypothesis and declares whether its identity is event-specific or build-level. Before reveal, set `embargoed: true` and omit diagnosis fields. Do not rewrite the frozen corpus after reveal. Put each `post_reveal_event`, the aggregate `post_reveal_causal_kind`, and the reclassification flag in the fresh holdout trial and diagnosis JSON. A single-event identity must reveal one causal event. A build-level identity may reveal several. Use `source_revision_status: unavailable` when build metadata is embargoed, or `branch_tip_only` when a branch tip is recorded solely as context. Never present a branch tip as the build revision. Do not record a guessed diagnosis.

Every evidence item names an artifact, source, or metadata path; bounded lines when available; and the exact claim it supports. Record competing hypotheses and passing comparison evidence explicitly. An assigned owner requires positive component-specific evidence; exculpatory evidence can exclude a failed operation without proving another component owns the failure. Preserve open handoffs. Record transient status as `true`, `false`, or `unresolved`. Keep same-run and cross-run evidence IDs separate. Cross-run success can bound recurrence but cannot by itself prove that the failed run recovered. Keep raw logs and private model content out of the corpus.

## Extract prompt knowledge

Promote a fact into `prompts/system.md` only when it is supported by pinned source or more than one relevant case and is useful beyond one exact build.

Classify candidates as:

- **Stable architecture:** component, API, resource, or ownership relationship.
- **Stable lifecycle:** ordered phases and the evidence that proves progress.
- **Job or flavor distinction:** behavior that must be gated on effective job identity.
- **Artifact contract:** stable path or path pattern plus what it proves.
- **Failure rule:** signal, required evidence, competing inference, and bounded remediation or ownership conclusion.
- **Transient rule:** same-run recovery evidence, separate cross-run context, and the non-transient boundary.
- **Recurrence signature:** a narrow, highly specific symptom or identity pattern retained separately from stable project facts.
- **Unresolved:** important knowledge not established by the corpus.

Do not put a narrow failure procedure into the prompt merely because it occurred repeatedly in one job. Require an independent counterexample search first.

Before drafting, inventory the existing consumer prompt. Treat each rule as a candidate stable fact, recurrence signature, or unsafe/unverified claim. Verify it against pinned source or current evidence, then retain, update, remove, or defer it in `prompt_regression`. Record the job, build, test, event, and hashed source or prior report that established each historical rule under `baseline_provenance`. Absence from the new sample is not evidence that a previously verified stable rule is false. Do not treat existing consumer recipes as trusted quality exemplars. Re-check trigger scope, evidence relationships, counterexamples, and ownership boundaries.

## Validate the prompt with fresh sessions

Prompt validation is prompt-first and recipe-free.

For each validation case:

1. Start a fresh isolated LLM CLI or agent session only in a user-approved environment. Send the minimum public or explicitly authorized evidence. Never transmit private source or artifacts without explicit authorization. Give it the proposed prompt, pinned case identity, raw artifacts, and bounded source access, but not the corpus diagnosis or expected answer.
2. Record its initiating error, causal chain, tri-state transient decision, same-run and cross-run evidence, citations, ownership discipline, storage identity correlation when applicable, and unresolved evidence.
3. Compare that output with the artifact-backed case diagnosis.
4. Revise only stable prompt guidance. Do not add the exact validation answer or build-specific terms.
5. Re-run all earlier validation cases after a revision to detect regressions.

Use no more than two bounded revision rounds by default. Additional rounds need a new validation case or a documented reason. If fresh sessions are unavailable, perform the deterministic corpus and prompt review, mark independent prompt validation unavailable, and do not claim generalization.

Record authoring validation separately from the dashboard provider benchmark. A Copilot CLI, Codex, Claude Code, or similar fresh-session result can improve the prompt-authoring process, but it does not replace the engine A/B/C benchmark.

## Apply the prompt quality rubric

Score the proposed prompt before freezing it. Require:

- Coverage of the selected job families and lifecycle phases.
- A pre-test Prow branch for unscheduled pods and missing execution artifacts.
- Retention or evidence-backed removal of verified existing prompt knowledge.
- Exact grounded component, resource, API, and repository relationships.
- Artifact paths paired with what they prove and cannot prove.
- Initiating errors separated from terminal wrappers and cleanup noise.
- Competing-cause boundaries for repeated wrappers.
- Same-run progress for `true` transient claims, separate cross-run evidence, and explicit non-transient boundaries.
- Storage triage that separates scheduling, binding, attach, stage, publish, Running state, and later statistics assertions by timestamp.
- No unavailable live-cluster, shell, browser, portal, or SSH instructions.
- No build IDs, generated object instances, expected diagnoses, or validation answers.
- Concise operational rules rather than repository documentation.
- Important unknowns retained under `## Unresolved details`.

A prompt that passes heading validation but fails this rubric is not ready for a holdout.

## Stop when evidence is insufficient

Do not write project-wide causal rules when:

- The corpus contains only one initiating-error class.
- All examples share one wrapper and no competing cause was checked.
- Artifact paths are inferred rather than observed or source-grounded.
- Passing or recovery evidence needed for a transient rule is unavailable.
- The prompt works only after adding the validation answer verbatim.

In these cases, improve architecture, lifecycle, artifact, and unresolved sections only. Classify narrow prompt changes or recipes `unresolved`, or abstain.
