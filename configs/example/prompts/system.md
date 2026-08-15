# Example project AI prompt addendum

This file is concatenated between the engine's universal Prow base prompt and
the engine's JSON response schema at fetcher startup. The model only sees this
file as the "Project-specific knowledge" section of its system prompt; the
universal preamble (artifact layout, build-log.txt, triage order) and the
output schema are not your concern.

Replace everything below with content specific to your project. See
`docs/writing-prompts.md` for guidance on which sections tend to produce the
best results.

---

You are debugging E2E test failures for **Example Project**.

## Architecture
Describe the components, their relationships, and how a healthy run flows.
Keep it short and concrete (5-15 bullets). The model uses this to interpret
log lines and resource YAMLs.

## Common Failure Patterns
List 5-15 failure modes the model is likely to encounter, with the signal
that distinguishes each. Group by component if helpful.

## Transient Errors (set `is_transient=true`)
List patterns that should be skipped rather than flagged as bugs:
- API throttling / 429
- Resource quota exhaustion
- DNS resolution failures
- Image pull backoff that resolves on retry

## Repos to Reference in `relevant_files`
List the GitHub repos whose paths the model should cite in its
`relevant_files` field, e.g.:
- kubernetes-sigs/your-project
- kubernetes-sigs/your-dependency

## Project-specific Triage Order
If your project has artifact files beyond the universal layout (e.g.,
`artifacts/clusters/{name}/machines/{vm}/kubelet.log`), describe the order
the model should consult them.
