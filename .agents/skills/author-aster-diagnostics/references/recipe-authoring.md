# Recipe authoring and applicability validation

## Contents

- [Use the current engine contract](#use-the-current-engine-contract)
- [Design triggers](#design-triggers)
- [Design evidence groups](#design-evidence-groups)
- [Write the procedure](#write-the-procedure)
- [Build the applicability matrix](#build-the-applicability-matrix)
- [Run deterministic engine validation](#run-deterministic-engine-validation)

## Use the current engine contract

A recipe is a second-line intervention. Before authoring YAML, require at least two prompt-only misses from separate validation-set causal events and separate fresh sessions for the same evidence relationship. The final holdout must never be used to justify or revise a proposal. Duplicate retries, duplicate JUnit entries, or repeated builds of one unchanged signature count once. Record case IDs, causal-event IDs, fresh-session IDs, and the prompt hash. If prompt guidance was incomplete, revise the prompt instead of creating a recipe. Treat every existing consumer recipe as untrusted input rather than a quality exemplar. A broad installed trigger or procedure does not satisfy the miss threshold, evidence standard, or applicability matrix.

Read the selected engine revision's `docs/skills.md` and `backend/internal/ai/skills/skills.go` before authoring. They are the source of truth for fields, matching, ordering, profiles, evidence satisfaction, and hashing. Do not copy an old schema example into a consumer.

The current recipe contract has no recipe-level schema version field. Record `recipe_schema_version` as `not_applicable` plus the exact engine commit in the report. Do not invent a `version` key. Strict YAML decoding rejects unknown fields.

Use a consumer ID that is unique across the merged engine and consumer set. Never use the reserved `engine.` prefix. Keep IDs stable and meaningful.

## Design triggers

Triggers participate in two phases. Before iteration one, the engine matches them against a bounded failure signal built from the test or build name, location, JUnit path, failure message, and tail of the failure body. During critique, it matches them against the joined root cause, summary, suggested fix, and relevant files. Test both inputs. A recipe may match only one phase, but an initial evidence plan requires a safe failure-signal match.

A trigger should identify the failure relationship whose evidence the recipe can check. Triggers are ORed. Priority changes order but does not suppress another matching recipe, so validate the complete merged set rather than treating a specific recipe as an override.

Treat a final-draft-only trigger as high risk because it can activate after the model invents the relationship and then reinforce that draft. Do not classify such a proposal `recommended` without repeated final-holdout evidence. Prefer a safe initial failure-signal match plus evidence that distinguishes the closest competing cause.

Prefer a small number of relationship-aware regular expressions. Bind the subject, operation, error, and direction within a bounded span where practical. Avoid single broad terms such as `timeout`, `failed`, `controller`, `deployment`, `mount`, or `scheduler`.

Reject these shapes unless a more specific relationship also appears:

- A negated claim.
- The intended cause and effect in reverse order or reversed meaning.
- A success response described near failure vocabulary.
- A terminal timeout, wrapper, cleanup error, or test-harness summary alone.
- Another project's resource with shared vocabulary.

Do not embed exact build IDs, generated object names, pod names, expected answers, or one observed message when the stable relationship is broader.

## Design evidence groups

Use stable artifact-path regexes established by source or repeated manifests. The engine slash-normalizes and lowercases artifact paths before path-regex matching. Write and test path patterns for that normalized input. Content regexes remain case-sensitive unless they include an explicit flag such as `(?i)`.

Each group should answer one necessary question. Prefer two or three focused groups over one broad group. Use `content_any_of` or `content_all_of` when path presence alone cannot ground the claim. Content proof must come from the same artifact whose path matched `any_of`.

Distinguish unavailable evidence from available but unread evidence. Absence is proven only by a complete, untruncated artifact tree. A matching candidate that exists but was not read remains unmet.

Use `when` only when the recipe legitimately covers distinct draft classes. Ensure a missing or unread group causes abstention or bounded uncertainty rather than a forced diagnosis.

Required evidence should be capable of disproving the hypothesis. Do not require only evidence that repeats the candidate answer. When ownership depends on a component boundary, require positive successful-operation evidence and correlate the same object identity across layers, not merely an error or missing path. For storage, require same-volume, same-PVC, same-pod, same-node, and same-time-window correlation. If any dimension is missing, preserve an open handoff rather than assigning ownership. For a dependency cascade or terminal wrapper, include evidence for the closest competing initiating cause, or require the procedure to abstain when that evidence is absent. Recipe evidence participates in planning and critique, but missing recipe evidence is not a universal hard cache barrier. Record the consumer's effective critique cache policy and verify actual published and cache behavior instead of claiming that a recipe guarantees rejection.

## Write the procedure

Keep the procedure short, ordered, and tool-oriented. Tell the analyzer which artifact relationship to establish and which alternatives to distinguish. Do not tell it what conclusion to produce. Treat exact commands and flags as security-sensitive grounding because the engine may accept a flag mentioned by a matched procedure as supported output. Include only commands established by pinned project evidence.

Include these safeguards when relevant:

- Separate initiating errors from terminal wrappers.
- Compare request and response, producer and consumer, or controller and object state before assigning ownership.
- Record transient status as `true`, `false`, or `unresolved`.
- Keep same-run evidence separate from cross-run context. Only same-run later success or forward progress establishes `true`.
- State the non-transient boundary and unresolved reason when needed.
- Abstain when required evidence is unavailable or contradictory.

## Build the applicability matrix

Record every case with a stable ID, corpus split, linked prompt-only miss case IDs, distinct causal-event IDs, distinct fresh-session IDs, input failure signal or draft text, artifact paths and bounded content, expected matched recipe IDs, expected applicable evidence groups, and expected satisfaction result.

Cover at least:

1. Positive causal match.
2. Unrelated negative.
3. Negated statement.
4. Reversed causal relationship.
5. Missing required evidence.
6. Partial evidence.
7. Shared-dashboard or multi-project collision.
8. Overlapping vocabulary with another recipe.
9. Normalized path matching and content case-sensitivity boundary.
10. Terminal wrapper that must not independently trigger.

Add successful-response and contradictory-evidence cases when the failure class contains request or API-response claims. A lexical match without the intended relationship must fail.

## Run deterministic engine validation

Validate proposals without activating them in the real consumer.

1. Create a disposable clean copy of the consumer.
2. Copy proposed recipes into that copy's active `skills/` only for validation.
3. Run the current engine's `onboard doctor` against the disposable copy for consumer configuration, prompt presence, deployment guidance, and real discovery. Doctor does not validate recipe YAML in the current engine.
4. Run the existing recipe package tests:

```bash
cd <engine>/backend
go test ./internal/ai/skills -count=1
```

5. Exercise the proposal matrix through the current engine implementation. If the engine revision has no proposal-test CLI, create one temporary Go test only in the disposable validation engine clone under `backend/internal/ai/skills`. Load the disposable consumer with `LoadForTools` and call the existing `ParseAndValidate`, `LoadMerged`, `Match`, `Plan`, `EvidenceGroup.Applies`, and evidence satisfaction methods. This is the loader check for unknown fields, duplicate IDs, reserved prefixes, selected profiles, regex compilation, and merged-set hashing. Do not reimplement regex or evidence semantics. Run only that test, then remove the temporary file from the validation clone. The pinned engine checkout must remain untouched.
6. Record the exact command, engine SHA, case results, merged IDs, selected profiles, and full skill-set hash.

A temporary test must contain only the frozen proposal paths and the explicit matrix. It must not contain locked diagnoses or final-holdout answers. If the engine lacks a stable non-test command for this workflow, record that as a generic engine gap instead of adding a new production package during authoring.
