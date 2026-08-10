# Agent Sandbox causal critic

Status: private, disabled-by-default experimental implementation. It is wired to
the scheduled fetcher and worker only when explicitly enabled. A public
immutable test image and an AKS evaluation exist, but the image is not part of
the release workflow and the critic is not approved for production use.

## Authority boundary

The in-process analyzer and its deterministic publication gates remain
authoritative. The independent critic reviews only one frozen evidence bundle
and one already selected draft. It cannot mutate publication, cache acceptance,
issues, fixes, notifications, corrections, remediation, or resolution state.

The fetcher invokes it only after authoritative output is complete. Results go
to a separate private ledger and never enter public dashboard JSON.

## Shared lifecycle

`backend/internal/agentsandbox` defines the business-neutral workload contract.
The Agent Sandbox adapter implements that interface and owns create or adopt
behavior, status polling, bounded Pod-log retrieval, timeouts, cancellation,
UID-checked cleanup, and orphan detection.

Fix PR generation remains an adapter above that lifecycle. The causal critic has
a separate request and result contract and does not reuse patch or changed-file
fields.

## Critic contract

`backend/internal/causalcritic` seals every trial to:

- the validated frozen `agentanalysis.EvidenceBundle`;
- the selected authoritative summary, root cause, classification, fix, and
  validated artifact citations;
- bounded cited evidence lines;
- bounded high-specificity error lines;
- bounded later-success counterevidence; and
- separate evidence, draft, and paired SHA-256 identities.

A successful review can return only `pass` or `object`. Objections use the
generic finding classes already used by semantic review. Every objection must
reference exact lines in the frozen bundle. Dashboard code deterministically
validates the schema, pair identity, finding classes, bounds, and evidence
references. No model call participates in result acceptance.

The private ledger claims an exact pair before execution, rejects duplicate
attempts, retains a valid review when cleanup remains pending, and records
lifecycle, gateway usage, resource, and finalization telemetry separately.

## Executor boundary

`backend/cmd/criticexecutor` and `backend/internal/criticexecutor` implement a
purpose-built executor. It has no repository checkout, GitHub credential,
provider credential, Kubernetes credential, shell, coding-agent harness, file
editing, web tool, artifact browser, or delegation mechanism.

The request arrives as one bounded environment value. The executor makes one
OpenAI-compatible chat-completions request to the configured consumer gateway
and emits one structured JSON result through stdout. The Agent Sandbox adapter
retrieves stdout through its bounded Pod-log result channel. Critic workloads do
not receive the writable workspace or temporary volumes required by Fix PR
execution.

Build the local image without publishing it:

```bash
make agent-sandbox-critic-executor-image
hack/test-agent-sandbox-critic-image.sh \
  ghcr.io/willie-yao/prow-ai-dashboard/agent-sandbox-critic-executor:dev
```

Model identity and token usage are copied only from the gateway response. An
optional gateway extension can report provider identity and cost. Missing fields
remain explicitly unavailable; the executor does not infer them from requested
configuration.

## Gateway authentication and isolation

The critic contract accepts only an internal HTTPS gateway and rejects direct
provider endpoints. It sends no Authorization, API-key, OAuth, or provider
credential header and rejects redirects.

This does not by itself authenticate the Sandbox to the gateway. Before a
cluster evaluation, the consumer must provide an infrastructure-level control
that is unavailable to the executor process. Suitable examples include ambient
service-mesh workload identity or gateway authorization based on the critic
ServiceAccount identity. Namespace reachability alone is not sufficient.

Provider credentials remain exclusively in the gateway. The Helm integration
requires deny-by-default critic network policy. Standard Kubernetes peer
selection is the default. `mode: cilium` is available for AKS Cilium/Kata and
permits only cluster DNS plus the configured gateway port to cluster identities.
The gateway must separately authorize the exact critic ServiceAccount. Unlike
the Fix PR executor, the critic has no public repository egress requirement.

## Helm boundary

`agentSandbox.causalCritic.enabled` defaults to `false`. Enabling it requires:

- an existing Agent Sandbox installation and execution namespace;
- a secure RuntimeClass;
- an immutable critic executor image digest;
- a tokenless critic workload ServiceAccount;
- an internal HTTPS gateway;
- a separate private ledger PVC;
- exact DNS and gateway NetworkPolicy settings; and
- in-process authoritative analysis.

The chart adds separate critic RBAC, admission, and NetworkPolicy resources. The
critic admission policy requires exactly one non-root executor container, no
volumes, no extra containers, no host access, one bounded request environment
value, immutable resources and image, and the `critic` workload label. It does
not broaden the Fix PR admission contract.

## Finalization and cleanup

A parsed, valid review is preserved when Sandbox cleanup remains pending. The
caller receives both the review and cleanup error so the private ledger can
record diagnostic output without claiming finalization. A critic trial is fully
finalized only after:

1. the Sandbox result is received;
2. the result parses;
3. deterministic contract and evidence-reference validation passes;
4. UID-checked Sandbox and Pod cleanup is confirmed; and
5. the private comparison record is durably persisted.

Cleanup failure remains visible even when the review itself is valid.

## Cold comparison harness

The opt-in benchmark consumes private in-process benchmark JSONL and runs the
critic against the same selected case and authoritative draft. It records the
frozen evidence hash, draft hash, pair hash, lifecycle status, findings, signal
coverage, requests, tokens, cost, duration, and cleanup state.

```bash
RUN_AGENT_SANDBOX_CAUSAL_CRITIC_BENCHMARK=1 \
BENCH_CASE=<case-id> \
BENCH_EVIDENCE_CONDITION=fixture-v1 \
CRITIC_BENCH_INPROCESS_JSONL=/private/inprocess.jsonl \
CRITIC_BENCH_RESULTS_JSONL=/private/critic.jsonl \
CRITIC_BENCH_LEDGER_PATH=/private/critic-ledger.json \
CRITIC_BENCH_KUBE_CONTEXT=kind-prow-ai-shadow-bench \
AGENT_SANDBOX_CRITIC_NAMESPACE=<namespace> \
AGENT_SANDBOX_CRITIC_IMAGE=<image@sha256:digest> \
AGENT_SANDBOX_CRITIC_SERVICE_ACCOUNT=<service-account> \
AGENT_SANDBOX_CRITIC_RUNTIME_CLASS=<runtime-class> \
AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_ENDPOINT=<internal-https-url> \
AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_MODEL=<model> \
AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_PROTOCOL=openai-chat-completions-v1 \
AGENT_SANDBOX_CRITIC_MODEL_GATEWAY_PUBLIC_CA_PRIVATE_DNS=false \
AGENT_SANDBOX_CRITIC_TIMEOUT=5m \
AGENT_SANDBOX_CRITIC_OUTPUT_LIMIT_BYTES=65536 \
AGENT_SANDBOX_CRITIC_POLL_INTERVAL=250ms \
AGENT_SANDBOX_CRITIC_CPU_REQUEST=50m \
AGENT_SANDBOX_CRITIC_CPU_LIMIT=500m \
AGENT_SANDBOX_CRITIC_MEMORY_REQUEST=64Mi \
AGENT_SANDBOX_CRITIC_MEMORY_LIMIT=256Mi \
AGENT_SANDBOX_CRITIC_EPHEMERAL_STORAGE_LIMIT=32Mi \
go test ./internal/e2e -run TestAgentSandboxCausalCriticBenchmark -v -timeout 60m
```

Generate a private JSON summary with:

```bash
RUN_AGENT_SANDBOX_CAUSAL_CRITIC_REPORT=1 \
CRITIC_BENCH_RESULTS_JSONL=/private/critic.jsonl \
CRITIC_BENCH_SUMMARY_JSON=/private/critic-summary.json \
go test ./internal/e2e -run TestAgentSandboxCausalCriticBenchmarkReport -v
```

Use repeated cold authoritative records. A successful Sandbox execution is not
a promotion signal. The independent critic must catch meaningful causal gaps
beyond the same-model judge without unacceptable malformed, unavailable,
timeout, cost, latency, or cleanup regressions.

## Evaluation status

The August 9, 2026 AKS evaluation validated immutable digest execution, Kata
isolation, RuntimeDefault AppArmor and seccomp, tokenless workload identity,
admission denial cases, identity-gated gateway ingress, public-egress denial,
UID-checked cleanup, and no leaked Sandboxes. The public test image was
published manually for that evaluation. It remains intentionally absent from
the release workflow.

The first cold comparison matrix produced 10 valid and finalized reviews from
15 trials, with cleanup succeeding in all 15. The Flatcar control produced no
valid reviews, and the Kueue oracle did not reliably recover the complete
initiating API-version chain. Fixture requests also consumed roughly 20K to 29K
input tokens and commonly took 30 to 60 seconds.

These results validate the runtime boundary, not diagnostic promotion. The
critic remains disabled, private, sampled, and non-authoritative. Further work
must improve structured validity, reduce critic-specific evidence size, and
show repeated causal gains over the in-process analyzer before a separate
production-enablement decision.
