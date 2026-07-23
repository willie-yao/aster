# Orka container analyzer assessment

Status as of July 23, 2026.

The container analyzer tests whether Orka should own lifecycle for the
dashboard's existing `FailureAnalyzer`. It does not use Orka's AI worker,
Provider, or Tool resources. The in-process analyzer remains canonical.

## Prototype evidence

The initial local kind prototype demonstrated:

- One dashboard-owned analyzer in an Orka `type: container` Task.
- The scripted Flatcar benchmark scored 5 of 5 expected diagnosis signals.
- Orka retried a failed analyzer attempt.
- Analyzer and helper workloads stayed on CPU nodes.
- The Task result remained available through Orka's result API.

This proves the basic lifecycle shape. It does not make the path production
ready.

## Production gates

| Gate | Status |
| --- | --- |
| Structured result framing | Implemented by `dashboard-failure-analyzer-v2` |
| Immutable request and project bundle | Pending |
| Persistent cache and private traces | Pending |
| Clean in-process and container Kimi comparison | Pending |
| Bounded multi-failure load test | Pending |

No supported Helm analysis mode should use the container path until every gate
passes.

## Result contract

Contract v2 emits exactly one dashboard-owned result marker on stdout:

```text
PROW_AI_RESULT_B64:<base64 FailureAnalysisResult JSON>
```

Normal analyzer logs and errors remain on stderr. The parser scans mixed Task
logs for the last valid marker instead of treating the final non-empty line as
JSON. Identical duplicate markers are accepted. Conflicting valid markers are
rejected. Missing markers, malformed base64, oversized payloads, malformed or
unknown JSON fields, and empty summaries are rejected.

The decoded result is limited to 2 MiB. The Orka controller can continue storing
combined pod logs without becoming the owner of the dashboard result schema.

## Remaining design risks

- The current request uses a bounded inline environment value and the benchmark
  image bakes in a project bundle.
- One-shot Tasks lose local cache and trace files unless the dashboard adds an
  explicit persistence transport.
- Orka adds scheduling and image startup overhead for every failure.
- The container adapter is useful only if operators value Task retry, history,
  placement, cancellation, and durable lifecycle state enough to justify the
  extra control plane.

## Next evaluation

Add a content-addressed request and project bundle without credentials. Then add
cache and trace persistence before running the Kimi parity and load tests. If
those steps require several custom services or sidecars, remove Orka analysis
and keep the in-process path.
