# Orka ai-worker patches (convergence + critique)

Changes to Orka's `workers/ai/main.go` (Orka's own source, not this repo) that the
migration needs. They are kept here as a patch because they live in the upstream
Orka worker; apply against a matching Orka checkout, rebuild the ai-worker image,
and load it into the cluster.

`ai-worker-convergence.patch` contains four changes, all in the agent loop:

1. **Iteration budget (from F1a):** `maxIterations` 10/50 -> 80. Open-weight models
   investigate less efficiently than Claude and exhausted a 50 cap without
   concluding on complex CAPZ failures.

2. **Forced finalization near the budget (G-Converge):** for the last 2
   iterations, the request is sent with NO tools plus a "budget exhausted, output
   your final JSON now" message. This guarantees a no-tool-call response the loop
   treats as completion, so a model that keeps investigating still emits an answer
   instead of erroring at the cap.

3. **Empty-final re-prompt (G-Converge, the decisive one):** when the model returns
   no tool calls AND empty content (a small-model failure mode - it terminates
   without answering), the loop re-prompts once for the final JSON instead of
   accepting an empty result.

4. **Transient critique gate (G-Critique):** when the final answer sets
   is_transient=true but the model never called verify_timeline, the loop
   re-prompts (up to twice) with the engine's contract: confirm the failing
   operation was actually dropped/never-registered with verify_timeline, or set
   is_transient=false (default to a real bug). A bare "context deadline exceeded"
   or signature match is background noise, not proof.

## Why (measured, Orka on gemini-3.5-flash, same 15 A2 failures)

G-Converge (valid final analyses):

| Worker | Valid analyses |
|---|---|
| baseline (cap 80 only) | 6/15 (40%) |
| + forced finalization | 9/15 (60%) |
| + empty-final re-prompt | **15/15 (100%)** |

The empty-final re-prompt was decisive: the failing tasks were not looping to the
cap, they terminated early with an empty message (~30 tool calls, then an empty
final turn -> "empty request body"). This closes the F3 same-model convergence gap.

G-Critique (transient verdicts that consulted verify_timeline):

| Worker | Discipline |
|---|---|
| before the gate | 3/8 |
| with the gate | **11/11** |

Important honest result: the gate deterministically enforces the DISCIPLINE (every
transient verdict now consults verify_timeline) but does NOT fix CLASSIFICATION on
a weak model. On gemini the azl3 etcd-join cases (f05/f06/f08) stayed mislabeled
transient: the model now calls verify_timeline but still misreads it. The gate
guarantees the model looks at the timeline; it cannot make a weak model reason
correctly about what it sees. Classification correctness on the ambiguous ~two
thirds of failures remains model-bound (claude gets them right; gemini does not).
The gate's value is a model-independent process guardrail - a transient claim must
be grounded in a timeline check - which matters most with a capable model.

## Upstreaming

These belong upstream in Orka (the worker should not silently submit an empty
result; forced finalization and a transient-critique re-prompt are generally
useful). Until then, carry this patch. A cleaner long-term form: make the
finalization reserve count, the empty-final re-prompt, and the critique-retry
budget configurable via the coordination env.
