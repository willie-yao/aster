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

Use engine discovery and public Prow artifact indexes to enumerate candidate
failures. Do not select only the first recent failures or only builds that share
one terminal wrapper.

When the project has enough history, target:

- At least six diagnosed failures.
- At least two jobs, job flavors, or shards.
- At least three initiating-error classes or lifecycle phases.
- At least one same-wrapper, different-cause counterexample for every proposed
  causal rule.
- A nearby passing build or later-success comparison for each transient rule
  when one is available.

These are corpus-quality targets, not reasons to invent data. Record the achieved
coverage and every missing dimension. If the project is smaller, use all
available independent cases and narrow the claims accordingly.

Prefer cases that expose different boundaries such as setup, controller
reconciliation, API compatibility, scheduling, node readiness, storage, test
assertion, and cleanup. Treat cleanup wrappers as a separate class only when they
are the initiating failure.

## Split cases before diagnosis

Assign every selected case to exactly one split before reading answer-bearing
material:

1. **Authoring set:** diagnose and use to draft the prompt.
2. **Validation set:** keep out of the initial prompt draft. Use for bounded
   prompt revision after the first draft.
3. **Final holdout:** keep embargoed until the prompt and proposals are frozen.

Keep the split at the build level. Related retries, duplicate artifacts, or two
JUnit entries from the same causal event belong to the same split.

Do not move a failed validation or final-holdout case into the authoring set and
then count it as independent. Revisions based on validation cases are allowed,
but the final holdout remains one-shot evidence.

## Diagnose each failure

Use already-collected artifacts. Do not execute project source, tests, or
binaries.

For every authoring or revealed validation case:

1. Read `prowjob.json` for the effective job, refs, environment, commands, and
   timing.
2. Read the exact JUnit failure or build-level failure signal.
3. Trace `build-log.txt` backward from the terminal error to the earliest
   initiating error and forward to determine whether recovery occurred.
4. List the artifact tree before declaring expected evidence absent.
5. Read the component, controller, workload, node, or API artifacts that can
   prove or disprove the leading hypotheses.
6. Read bounded source ranges at the pinned revision only when they establish
   stable control flow, ownership, artifact production, or API relationships.
7. Compare a passing neighbor, later-success transition, or independent
   same-wrapper case when available.
8. Record competing hypotheses and the evidence that rejects or leaves each one
   unresolved.

Do not stop at a timeout, generic exit status, cleanup error, or the last log
line. Do not infer ownership from timing proximity or a resource name.

## Write the deterministic corpus

Write `reports/failure-corpus.json` with `schema_version: 1`. Use stable arrays
and enum-like fields rather than hiding results in prose.

Each case should contain at least:

```json
{
  "id": "stable-case-id",
  "split": "authoring",
  "job_name": "periodic-example",
  "build_id": "123",
  "test_name": "Prow job execution",
  "source_revision": "<sha>",
  "failure_class": "api_compatibility",
  "phase_reached": "dependency_deployment",
  "initiating_error": "bounded factual statement",
  "terminal_wrapper": "bounded factual statement",
  "causal_chain": [],
  "evidence": [],
  "competing_hypotheses": [],
  "passing_comparison": null,
  "is_transient": false,
  "transient_evidence": [],
  "non_transient_boundary": "bounded factual statement",
  "reusable_project_facts": [],
  "prompt_candidates": [],
  "recipe_candidate": null,
  "unresolved": []
}
```

Use `split` values `authoring`, `validation`, or `final_holdout`. Before the
final holdout is revealed, record only its pinned identity and embargo status.
Do not record a guessed diagnosis.

Every evidence item should name the artifact or source path, bounded line or
range when available, and the claim it supports. Keep raw logs and private model
content out of the corpus.

## Extract prompt knowledge

Promote a fact into `prompts/system.md` only when it is supported by pinned
source or more than one relevant case and is useful beyond one exact build.

Classify candidates as:

- **Stable architecture:** component, API, resource, or ownership relationship.
- **Stable lifecycle:** ordered phases and the evidence that proves progress.
- **Job or flavor distinction:** behavior that must be gated on effective job
  identity.
- **Artifact contract:** stable path or path pattern plus what it proves.
- **Failure rule:** signal, required evidence, competing inference, and bounded
  remediation or ownership conclusion.
- **Transient rule:** same-run recovery evidence plus the non-transient boundary.
- **Unresolved:** important knowledge not established by the corpus.

Do not put a narrow failure procedure into the prompt merely because it occurred
repeatedly in one job. Require an independent counterexample search first.

Use strong existing consumer prompts only as structural and depth exemplars.
Never copy their project facts, paths, APIs, failure rules, or ownership claims.

## Validate the prompt with fresh sessions

Prompt validation is prompt-first and recipe-free.

For each validation case:

1. Start a fresh isolated LLM CLI or agent session only in a user-approved
   environment. Send the minimum public or explicitly authorized evidence. Never
   transmit private source or artifacts without explicit authorization. Give it
   the proposed prompt, pinned case identity, raw artifacts, and bounded source
   access, but not the corpus diagnosis or expected answer.
2. Record its initiating error, causal chain, transient decision, citations,
   ownership discipline, and unresolved evidence.
3. Compare that output with the artifact-backed case diagnosis.
4. Revise only stable prompt guidance. Do not add the exact validation answer or
   build-specific terms.
5. Re-run all earlier validation cases after a revision to detect regressions.

Use no more than two bounded revision rounds by default. Additional rounds need
a new validation case or a documented reason. If fresh sessions are unavailable,
perform the deterministic corpus and prompt review, mark independent prompt
validation unavailable, and do not claim generalization.

Record authoring validation separately from the dashboard provider benchmark.
A Copilot CLI, Codex, Claude Code, or similar fresh-session result can improve the
prompt-authoring process, but it does not replace the engine A/B/C benchmark.

## Apply the prompt quality rubric

Score the proposed prompt before freezing it. Require:

- Coverage of the selected job families and lifecycle phases.
- Exact grounded component, resource, API, and repository relationships.
- Artifact paths paired with what they prove and cannot prove.
- Initiating errors separated from terminal wrappers and cleanup noise.
- Competing-cause boundaries for repeated wrappers.
- Same-run progress for transient claims and explicit non-transient boundaries.
- No unavailable live-cluster, shell, browser, portal, or SSH instructions.
- No build IDs, generated object instances, expected diagnoses, or validation
  answers.
- Concise operational rules rather than repository documentation.
- Important unknowns retained under `## Unresolved details`.

A prompt that passes heading validation but fails this rubric is not ready for a
holdout.

## Stop when evidence is insufficient

Do not write project-wide causal rules when:

- The corpus contains only one initiating-error class.
- All examples share one wrapper and no competing cause was checked.
- Artifact paths are inferred rather than observed or source-grounded.
- Passing or recovery evidence needed for a transient rule is unavailable.
- The prompt works only after adding the validation answer verbatim.

In these cases, improve architecture, lifecycle, artifact, and unresolved
sections only. Classify narrow prompt changes or recipes `unresolved`, or abstain.
