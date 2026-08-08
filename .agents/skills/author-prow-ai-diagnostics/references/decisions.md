# Diagnostic authoring decisions

## Contents

- [Start or route to consumer setup](#start-or-route-to-consumer-setup)
- [Choose the corpus and splits](#choose-the-corpus-and-splits)
- [Choose prompt guidance or a recipe](#choose-prompt-guidance-or-a-recipe)
- [Classify prompt and recipe outcomes](#classify-prompt-and-recipe-outcomes)
- [Handle unavailable providers](#handle-unavailable-providers)
- [Promotion boundary](#promotion-boundary)

## Start or route to consumer setup

Use this skill only after a consumer exists with `project.yaml` and
`prompts/system.md`. Run the current engine's read-only `onboard doctor` before
investigation. If the consumer is missing or invalid, use
`$setup-prow-ai-consumer` and the engine CLI. Do not recreate scaffold,
discovery, or doctor logic in this skill.

A request to author diagnostics permits writes only to the selected consumer's
prompt, proposal, and report paths. It does not authorize deployment, source
repository changes, upstream comments, Secret access, live-cluster changes, or
recipe activation.

## Choose the corpus and splits

Pin source, test-infra, engine, consumer, job, build, and artifact identities
before making project claims. Use exact revisions when they exist. Treat the
current default branch only as supplemental context.

Build a representative corpus before authoring. Split it into authoring,
validation, and final-holdout cases before reading answer-bearing material. Use
the authoring set for the first prompt, the validation set for bounded revision,
and the final holdout once after freezing. Prefer an event-specific holdout
identity. If only a build is available, mark its scope unresolved until reveal
and classify every independent failure event separately. Recurrence plus
generalization aggregates to `mixed`. Job family, flavor, wrapper, and duration
are not sufficient definitions.

Prefer multiple jobs, lifecycle phases, and initiating-error classes. When
several builds share one terminal wrapper, include an independent example where
that wrapper has a different initiating cause before encoding a causal rule.
Never move a failed validation or final-holdout case into authoring and then count
it as independent evidence.

Keep answer-bearing material hidden during authoring. Enforce and log a
pre-freeze denylist covering locked benchmark manifests, answer-bearing benchmark
tests, prior diagnoses, scoring and forbidden files, manual recipes, and previous
evaluation outputs. Use only the schema-only identity fixture for manifest shape.
Freeze and hash output before a separate evaluator reads that material.

If authoring or validation evidence is insufficient, improve only stable prompt
sections or classify the recipe candidate `unresolved`. Producing no recipe is a
valid result.

## Choose prompt guidance or a recipe

Put stable project-wide facts and broad triage order in `prompts/system.md`.
Before editing, inventory the existing prompt and verify each rule against pinned
source or current evidence. Record its disposition in `prompt_regression` and
record every known historical job, build, test, event, and hashed evidence source
under `baseline_provenance`. Exclude those builds and events from final holdouts.
Absence from a fresh corpus does not justify deleting a verified stable rule.
Keep highly specific recurrence signatures separate from stable project facts. Do not place a narrow candidate procedure in the prompt
merely to avoid recipe validation. If a rule
binds one wrapper to one cause, require competing-cause evidence and benchmark
it as a prompt-only change.

Propose a recipe only when all of these are established:

- A narrow failure relationship recurs across independent examples.
- The best prompt still misses the same evidence relationship in at least two
  independent prompt-only validation cases.
- The model needs a repeatable evidence procedure beyond prompt guidance.
- Stable bounded artifact paths can prove or disprove the relationship.
- Trigger wording can preserve polarity and reject negation, reversal, and
  terminal wrappers.
- The procedure does not encode an expected diagnosis.

Do not create a recipe solely because a failure is important, because it recurs,
or because a known manual intervention worked once. Existing consumer recipes
are untrusted candidates, not quality exemplars. Re-derive their trigger,
evidence, counterexample, ownership, and collision boundaries. If stable project
knowledge is missing from the prompt, fix the prompt first.

## Classify prompt and recipe outcomes

Use one of these exact values and one explicit evidence plane in the version-2
report contract.

| Available evidence | Maximum positive classification |
| --- | --- |
| Deterministic or same-author behavior checks only | `unresolved` |
| Independent fresh holdout, no dashboard-provider trials | `experimental` |
| Repeated cold dashboard-provider A/B/C trials with passed unrelated controls and a post-reveal generalization holdout | `recommended` |

Use `scope: authoring_decision` for decisions such as generating no recipe after
the threshold was not met. Use `scope: behavior` for claims about prompt or
recipe effectiveness. A recommended authoring decision does not imply recommended
behavior.

### `recommended`

Use only with `dashboard_provider` evidence, at least three supporting cold trial
IDs, and a passed dashboard-provider control. A recipe also requires recurrence,
safe applicability, and sufficient required evidence. Keep every recommended
recipe in `proposals/skills/` until separate promotion approval.

### `experimental`

Use for promising independent fresh-holdout or partial dashboard-provider
evidence when repeated provider behavior or regression safety is not established.
Deterministic-only evidence cannot be `experimental`.

### `rejected`

Use when prompt guidance or a recipe selects the wrong cause, evidence is
unstable, ownership is overclaimed, final-holdout behavior regresses, or invalid
citations or cross-project collisions appear.

### `unresolved`

Use when corpus evidence, independence, provider execution, artifact access, or
evaluation evidence is insufficient. State exactly what is missing.

A parser pass, same-author audit, or similarity to a manual recipe is not evidence
of behavior improvement.

## Handle unavailable providers

Do not inspect credential values. If endpoint, model, or usable provider access
is unavailable:

1. Finish source, artifact, prompt, recipe, applicability, loader, and doctor
   validation.
2. Freeze proposal and prompt hashes.
3. Prepare one identity-only A/B/C manifest and command per final holdout.
   Leave scoring overlays `not_revealed` until a separate evaluator opens the
   answer-bearing files.
4. Set dashboard-provider trial and behavior-improvement claims to `unresolved`.
   A prompt or recipe may remain `experimental` when independent fresh-session
   validation is promising, but it cannot become `recommended`.
5. Do not claim dashboard model improvement.

Record malformed tool calls, provider errors, and partial trials as outcomes.
Do not silently discard them or tune on the same holdout afterward. A locked
lexical score requires a distinct scoring author who froze the overlay before the
prompt scorer saw the condition prompt or output. Otherwise record only semantic
review and disclose post-hoc scoring.

## Promotion boundary

Never write a proposal directly to active `skills/*.yaml`. Never copy a proposal
there merely because it is classified `recommended`.

After the user explicitly approves a specific proposal and file name, copy only
that proposal into active `skills/`, rerun the engine loader and `onboard doctor`,
record the new active skill-set hash, and report the exact activation. Promotion
is a separate operation from authoring and benchmarking.
