# Diagnostic authoring decisions

## Contents

- [Start or route to consumer setup](#start-or-route-to-consumer-setup)
- [Choose evidence and holdouts](#choose-evidence-and-holdouts)
- [Choose prompt guidance or a recipe](#choose-prompt-guidance-or-a-recipe)
- [Classify a proposal](#classify-a-proposal)
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

## Choose evidence and holdouts

Pin source, test-infra, engine, consumer, job, build, and artifact identities
before making project claims. Use exact revisions when they exist. Treat the
current default branch only as supplemental context.

Use at least two training examples for a recurring recipe candidate and reserve
independent held-out examples. Prefer multiple failure classes when improving
the general prompt. When several training builds share the same terminal wrapper,
include an independent example where that wrapper has a different initiating
cause before encoding a causal prompt rule. Never move a held-out case into
training to make a weak candidate pass.

Keep answer-bearing material hidden during authoring. This includes locked
reference diagnoses, expected dashboard answers, scoring regexes, manual
intervention recipes, and prior proposed recipes for the same holdout. Freeze
and hash proposals before a separate evaluator reads that material.

If independent training examples are insufficient, improve only the prompt or
classify the recipe candidate `unresolved`. Producing no recipe is a valid
result.

## Choose prompt guidance or a recipe

Put stable project-wide facts and broad triage order in `prompts/system.md`.
Examples include controller relationships, job families, artifact layout, and
transient boundaries that apply across several failures. Do not place a narrow
candidate procedure in the prompt merely to avoid recipe validation. If a rule
binds one wrapper to one cause, require competing-cause evidence and benchmark
it as a prompt-only change.

Propose a recipe only when all of these are established:

- A narrow failure relationship recurs across independent examples.
- The model needs a repeatable evidence procedure beyond prompt guidance.
- Stable bounded artifact paths can prove or disprove the relationship.
- Trigger wording can preserve polarity and reject negation, reversal, and
  terminal wrappers.
- The procedure does not encode an expected diagnosis.

Do not create a recipe solely because a failure is important or because a known
manual intervention worked once.

## Classify a proposal

Use one of these exact values in the deterministic report.

### `recommended`

Use only when recurrence, applicability, required evidence, repeated cold
held-out improvement, citation grounding, transient classification, and
cross-project controls all pass. Keep the file in `proposals/skills/` until a
separate promotion approval.

### `experimental`

Use when the hypothesis, trigger, and evidence procedure are defensible, but
repeated held-out improvement or regression safety is not established.

### `rejected`

Use when the trigger is unsafe, evidence is unstable, the procedure overclaims,
the candidate regresses held-out behavior, or it causes invalid citations or
cross-project collisions. Preserve the rejected proposal and evidence unless
the user asks to remove it.

### `unresolved`

Use when training evidence, independent holdouts, provider execution, artifact
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
4. Set model-backed trial status and recipe classification to `unresolved`.
5. Do not claim improvement.

Record malformed tool calls, provider errors, and partial trials as outcomes.
Do not silently discard them or tune on the same holdout afterward.

## Promotion boundary

Never write a proposal directly to active `skills/*.yaml`. Never copy a proposal
there merely because it is classified `recommended`.

After the user explicitly approves a specific proposal and file name, copy only
that proposal into active `skills/`, rerun the engine loader and `onboard doctor`,
record the new active skill-set hash, and report the exact activation. Promotion
is a separate operation from authoring and benchmarking.
