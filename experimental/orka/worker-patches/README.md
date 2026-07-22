# Orka AI-worker compatibility image

The dashboard publishes a worker-only Orka compatibility image for the
Orka backend preview. The image is built from a pinned upstream Orka
commit plus `ai-worker-convergence.patch`; it does not require an upstream Orka
PR or a patched controller, CRD, API server, or UI.

See [COMPATIBILITY.md](COMPATIBILITY.md) for the exact source revision, patch
checksum, image identity, and deployment instructions.

## Compatibility v5 behavior

The patch keeps dashboard analysis policy inside the dashboard-owned worker:

- recognizes `validate_analysis`, `submit_analysis`, and `verify_timeline`
  finalization Tools, including content-addressed resource names;
- constructs the final Task result only from a successful validated submission;
- bounds validated runs to 25 model iterations, reserving four of the 20
  evidence Tool calls for targeted validation repair;
- selects one Tool call per turn, prioritizing timeline verification and final
  submission;
- supports short model-facing Tool aliases while keeping approval and policy
  identity bound to the canonical Tool resource;
- reuses semantically identical calls only when an immutable Tool opts in;
- checks the request-scoped Tool allowlist before duplicate-result reuse;
- removes completed timeline Tools and re-prompts malformed, empty, or
  unvalidated final responses;
- when final validation reports complete `missing_evidence_candidates`, restores
  only the artifact readers plus finalization Tools, prioritizes a reader over a
  repeated submission, and allows up to four targeted evidence-repair calls;
- starts finalization early enough to allow timeline verification, an initial
  submission, all four repair calls, and one final submission turn, then directs
  the model to use ledger tokens instead of readers
  that are no longer advertised;
- tells weak models to preserve applicable source paths and use
  `"relevant_files": []` only when that required array has no entries, without
  weakening deterministic validation;
- resets the premature-final retry counter after any Tool call so an early
  tools-free response cannot poison a later validated submission;
- keeps a bounded evidence ledger containing successful Tool arguments,
  byte-exact evidence tokens, and result excerpts; when necessary it removes
  non-token context or evicts whole older entries rather than truncating tokens;
- proactively compacts old Tool-call/result blocks before provider rejection,
  then restores the evidence ledger and finalization prompt;
- uses the Responses API when the Provider supports it and retains Chat
  Completions fallback for compatible endpoints, including Copilot models that
  reject `/responses` with `model_not_supported`;
- sends `store: false` on Responses requests;
- records the negotiated API mode and response ID in completion events, worker
  logs, and OpenTelemetry spans.

The patch intentionally does not add durable event types, Task Trace states, UI
behavior, Task CRD fields, or controller policy. Parallel Tool calls that are not
selected remain worker-local and do not require a `ToolCallSkipped` event.

## Measured Kimi result

A Phase 2 run used `moonshotai/Kimi-K2-Instruct-0905` against the same DRA
failure from build `2078833416211533824`. The prompt included the bounded JUnit
failure body and filtered artifact tree. Compatibility v5 retains the prior
evidence and API-mode behavior while reserving final Tool capacity for targeted
validation repair.

| Metric | Phase 1c | Compatibility v2 baseline |
|---|---:|---:|
| Model requests | 22 | **13** |
| Tool calls | 22 | **13** |
| Tool failures | 3 | **0** |
| Input tokens | 644,937 | **286,833** |
| Context compactions | 0 | 2 |
| Runtime | 5m39s | 6m02s |


A pre-v5 Flatcar benchmark confirmed the deterministic evidence plan directed
Kimi to the affected MachineDeployment, Machine, and Node, but the model consumed
all 20 investigation calls before its first rejected submission. In a v5 run,
the first missing-evidence response re-enabled the readers and allowed exactly
four targeted calls before returning permanently to finalization. Kimi spent
those calls on repeated JUnit reads rather than covering every missing group and
still failed after two attempts. This verifies the reserved budget and no-resume
state machine; candidate selection within that budget remains model-bound.

Phase 1c described only a pod timeout and generic resource-allocation delay.
The v2 baseline traced the actual chain: the device-plugin stream ended with
EOF, kubelet removed the endpoint, and container creation failed with
`endpoint not found in cache for a registered resource`. Kimi still set
`is_transient=true`, so final classification remains model-limited even though
root-cause quality and request efficiency improved substantially.

## Build and validation

```bash
make orka-compat-check
make orka-compat-image ORKA_COMPAT_IMAGE=orka-ai-worker:local
```

Pull requests verify the pinned source and patch checksum, apply the patch to a
clean checkout, run provider, focused, and full worker tests, run the complete
worker race suite, render the pinned Helm chart, and build the `linux/amd64`
worker image without publishing. Merges to `main` publish an immutable tag
containing both the Orka and dashboard commits, with SBOM and provenance attestations.

## Ownership boundary

This compatibility contract is maintained by `prow-ai-dashboard`. Generic Orka
features may be proposed upstream separately, but dashboard-specific analysis
schema, budgets, finalization, timeline policy, and evidence retention stay in
this patch.
