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
and the final holdout once after freezing.

Prefer multiple jobs, lifecycle phases, and initiating-error classes. When
several builds share one terminal wrapper, include an independent example where
that wrapper has a different initiating cause before encoding a causal rule.
Never move a failed validation or final-holdout case into authoring and then count
it as independent evidence.

Keep answer-bearing material hidden during authoring. This includes locked
reference diagnoses, expected dashboard answers, scoring regexes, manual
intervention recipes, and prior proposed recipes for the final holdout. Freeze
and hash output before a separate evaluator reads that material.

If authoring or validation evidence is insufficient, improve only stable prompt
sections or classify the recipe candidate `unresolved`. Producing no recipe is a
valid result.

## Choose prompt guidance or a recipe

Put stable project-wide facts and broad triage order in `prompts/system.md`.
Examples include controller relationships, job families, artifact layout, and
transient boundaries that apply across several failures. Do not place a narrow
candidate procedure in the prompt merely to avoid recipe validation. If a rule
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
or because a known manual intervention worked once. If stable project knowledge
is missing from the prompt, fix the prompt first.

## Classify prompt and recipe outcomes

Use one of these exact values in the deterministic report.

### `recommended`

Use only when the prompt or recipe shows repeated cold final-holdout
improvement, grounded citations, correct transient classification, and no
cross-project regression. A recipe also requires recurrence, safe applicability,
and sufficient required evidence. Keep every recommended recipe in
`proposals/skills/` until separate promotion approval.

### `experimental`

Use when prompt guidance or a recipe hypothesis is defensible and validation is
promising, but repeated final-holdout improvement or regression safety is not
established.

### `rejected`

Use when prompt guidance or a recipe selects the wrong cause, evidence is
unstable, the procedure overclaims, final-holdout behavior regresses, or invalid
citations or cross-project collisions appear. Preserve rejected output and
evidence unless the user asks to remove it.

### `unresolved`

Use when corpus evidence, independent final holdouts, provider execution, artifact
access, or evaluation evidence is insufficient. State exactly what is missing.

A parser pass is not evidence of diagnostic improvement. Similarity to a manual
recipe is also not evidence of improvement.

## Handle unavailable providers

Do not inspect credential values. If endpoint, model, or usable provider access
is unavailable:

1. Finish source, artifact, prompt, recipe, applicability, loader, and doctor
   validation.
2. Freeze proposal and prompt hashes.
3. Prepare exact condition-specific benchmark manifests and commands.
4. Set dashboard-provider trial and behavior-improvement claims to `unresolved`.
   A prompt or recipe may remain `experimental` when independent fresh-session
   validation is promising, but it cannot become `recommended`.
5. Do not claim dashboard model improvement.

Record malformed tool calls, provider errors, and partial trials as outcomes.
Do not silently discard them or tune on the same holdout afterward.

## Promotion boundary

Never write a proposal directly to active `skills/*.yaml`. Never copy a proposal
there merely because it is classified `recommended`.

After the user explicitly approves a specific proposal and file name, copy only
that proposal into active `skills/`, rerun the engine loader and `onboard doctor`,
record the new active skill-set hash, and report the exact activation. Promotion
is a separate operation from authoring and benchmarking.
