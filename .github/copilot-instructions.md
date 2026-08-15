# Copilot code review instructions

## Review quality

- Prioritize high-confidence correctness, data-integrity, security, and operability issues. Avoid style-only suggestions unless they conflict with an established repository convention.
- If a concern depends on an unverified assumption, state that assumption and label the suggestion as optional instead of presenting it as a definite bug.
- Describe a concrete failing scenario and the observable impact for every finding. Do not comment only because code could be written differently.
- Read the relevant tests, API types, persisted state, frontend consumers, and documentation before concluding that one changed line is incorrect.
- Group findings by the underlying invariant. Before posting a comment, inspect parallel operations and surfaces for the same issue and identify all affected call sites together.
- Distinguish required correctness fixes from optional hardening. Label optional suggestions clearly and do not present them as bugs.
- Prefer an invariant-level recommendation over a sequence of local patches. Consider the new failure modes introduced by a suggested API or state-model change.

## Asynchronous and persistent workflows

When a change involves background work, polling, persistent requests, or multi-step mutations, review the complete state machine before submitting comments:

- Trace creation, pending work, success, failure, confirmation, cancellation, expiry, replacement, restart recovery, and cleanup.
- Check both the in-page UI and dedicated review pages, including create, refine, confirm, cancel, close, reload, and navigation paths.
- Treat late, duplicated, reordered, or lost HTTP responses as expected failure modes. Verify that retries cannot create duplicate external writes.
- Verify that request identity, action kind, preview data, status, error state, and result links remain consistent when responses arrive after navigation or another operation.
- Background polling must not reopen dismissed UI, overwrite a newer entity, or apply stale errors to recovered state.
- Replacement or supersession must be atomic or idempotent, cancel in-flight work before it publishes or notifies, and leave a durable way to discover the replacement.
- If a mutation response is lost, reload authoritative server state and derive the UI from that recovered state instead of preserving the transport error.
- Check expiry for every active status, not only completed drafts.
- Capabilities that are advertised independently must gate their controls independently. User messaging about optional notifications or integrations must remain conditional.
- Reject malformed or truncated request bodies when silently dropping fields would change the operation's meaning.
- Review persisted schema changes for restart behavior, old state files, cleanup, and bounded retention.

## Tests

- Request deterministic tests for new concurrency, cancellation, persistence, expiry, supersession, and lost-response invariants.
- For in-flight cancellation, verify that the cancelled worker cannot publish or notify after its replacement completes.
- Reuse existing helpers and test seams. Do not ask for broad test infrastructure when a focused package test proves the invariant.
- Check whether existing tests already cover the scenario before requesting another test.
