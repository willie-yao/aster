# Agent Sandbox Fix Runtime Productionization

Status: local implementation and final productionization lifecycle evaluation
completed on August 8, 2026. The work is based on
`985ed6481d9c980e133027dc0f1531c350693110`.

During local implementation and evaluation, no live dashboard, AKS cluster,
H100 environment, provider endpoint, GitHub issue or pull request, or published
image was modified.

## Decision

Agent Sandbox is the selected Fix PR execution runtime. The engine integration,
bounded OpenCode executor, optional Helm wiring, least-privilege RBAC,
fail-closed admission, and deterministic gateway are implemented locally.

The final structured-command production executor completed one cold Sandbox
lifecycle in a disposable kind cluster. Standard kind `runc` proves API,
execution, result, and cleanup integration only. It does not prove hostile-code isolation or AppArmor
enforcement.

## Architecture

The existing Fix PR pipeline remains authoritative:

```text
fixpr.Manager
  -> runtime.AgentRuntime.Generate
       -> Agent Sandbox adapter
            -> v1beta1 Sandbox
                 -> fixexecutor
                      -> direct provider or consumer model gateway
  -> independent diff reconstruction
  -> existing scope and verification gates
  -> preview or separately confirmed GitHub write
```

Agent Sandbox API types do not enter `fixpr` or the provider-neutral execution
contract.

## Provider-neutral contract

`backend/internal/runtime/execution_contract.go` defines version 2 of the
non-secret request and result protocol. Version 2 adds explicit provider
credential mode, API, and auth metadata. It carries no Secret reference,
credential value, or credential hash.

The request binds:

- a public unauthenticated HTTPS repository URL or an absolute local fixture;
- an immutable lowercase 40-character Git SHA;
- the exact expected base SHA;
- a bounded generation prompt;
- maximum execution steps and changed files;
- exact final validation argv commands;
- wall-clock timeout and serialized output limit; and
- a non-secret provider credential mode, API, endpoint, model, auth type, and fixed token environment name.

The result reports:

- exact base SHA;
- sorted changed files and complete staged content;
- unified diff;
- command exit code, duration, stdout, stderr, and timeout state;
- succeeded, failed, timed-out, or cancelled terminal state;
- duration and resource metadata; and
- a required reason for every non-success state.

The engine reapplies the returned patch to the pinned base. Existing policy
rejects deletions, renames, binaries, symlinks, submodules, mode changes, unsafe
paths, oversized content, and reported versus reconstructed output mismatches.

## Production executor

`backend/cmd/fixexecutor` and `backend/internal/fixexecutor` implement the
production workload. The image inherits OpenCode 1.18.2 from its official image
pinned at
`sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af`.

The executor:

1. validates the shared request;
2. anonymously clones the approved public repository;
3. checks out and verifies the immutable SHA;
4. removes the Git remote;
5. protects Git metadata while OpenCode runs;
6. writes an isolated OpenCode config containing no token and, for direct bearer mode, only a fixed environment reference;
7. disables Bash, web fetch, task delegation, external skills, and external
   directory access;
8. calls only the configured direct provider or explicit tokenless gateway;
9. verifies HEAD and remotes, then stages the patch;
10. snapshots the staged patch before final validation;
11. runs only the configured final commands;
12. verifies validation did not change HEAD, remotes, or staged content;
13. requires the final argv to be `["git", "diff", "--cached", "--check"]`; and
14. rejects exact credential-bearing output before emitting the bounded versioned result, without pushing or making a GitHub write.

## Provider credential modes

Agent Sandbox remains disabled by default. After the Fix runtime is explicitly
enabled, direct mode is the default. Direct bearer mode injects one dedicated
inference credential from an existing Secret in the execution namespace into
the executor as `PROW_AI_MODEL_PROVIDER_TOKEN`. Direct unauthenticated mode has
no Secret reference. Explicit gateway mode remains tokenless.

The chart and dashboard never read or copy the Secret value. The dashboard sees
only the exact Secret name and key needed to construct the Sandbox Pod.
Admission permits one `secretKeyRef` only in direct bearer mode and rejects
arbitrary environment entries, `envFrom`, Secret volumes, projected tokens, and
additional credential references. OpenCode reads the credential through
`{env:PROW_AI_MODEL_PROVIDER_TOKEN}`. The token is never written to
`opencode.json`.

The executor scans stdout, stderr, summary text, validation output, changed-file
content, the patch, the structured result, and failure text for the exact token.
A match replaces the result with a fixed sanitized failure. Runtime identity,
workload hashes, labels, annotations, and telemetry identify direct versus
gateway mode without reading or hashing the Secret value.

Use only a dedicated inference credential. GitHub write tokens, repository read
credentials, OAuth credentials, and general PATs remain outside the Sandbox.

## AppArmor capability decision

Production constructors always request `RuntimeDefault` AppArmor on both the Pod
and executor container. Production Helm admission pins both fields to
`RuntimeDefault`. Project configuration, chart values, environment variables,
and the execution request expose no AppArmor selector.

Docker Desktop kind does not provide AppArmor. The local evaluation therefore
uses an internal test-only capability that is constructed only from Go test
files. It omits AppArmor from both the normal preflight Pod and the Sandbox Pod.
The production constructor rejects that capability, and the capability cannot be
selected through deployed configuration.

One canonical workload builder produces the Pod spec used by both the Sandbox
and normal preflight Pod. The parity test compares the complete Pod specs and
then verifies that production differs from local kind only by the two
`RuntimeDefault` AppArmor fields. The capability also participates in the
content-addressed execution identity.

The harness performs two admission checks:

1. the production Helm policy accepts the production Sandbox with
   `RuntimeDefault` AppArmor in a server-side dry run;
2. a local test-only copy of that policy changes only the two AppArmor
   predicates to require the fields to be absent.

The local policy denies an attempted `Unconfined` profile. This local policy and
capability are evaluation artifacts, not chart settings.

Production AppArmor enforcement remains unvalidated. It must be validated on the
actual secure RuntimeClass and node operating system before deployment.

## Future shared lifecycle seam

The current low-level `agentSandboxAPI` interface is the raw Kubernetes client
boundary for create, state, delete, bounded logs, Pod existence, and execution
Pod discovery. The `AgentSandboxRuntime` above it currently owns create-or-adopt
recovery, polling, timeout and cancellation handling, UID-checked cleanup,
orphan detection, and terminal lifecycle classification.

If a later read-only causal critic also uses Agent Sandbox, the intended small
extraction is a package such as `backend/internal/agentsandboxruntime`. It would
move the raw client, state model, and those lifecycle operations together while
leaving Fix PR request validation, patch reconstruction, and result policy in
`fixruntime`. This PR does not add shadow analysis or perform that extraction.

## Agent Sandbox adapter

The adapter creates one cold `agents.x-k8s.io/v1beta1` Sandbox per request. Its
content-addressed identity includes the request, image, workload identity,
RuntimeClass, resources, and AppArmor capability.

The production Sandbox has:

- a tokenless workload ServiceAccount;
- non-root UID/GID 65532 and fsGroup 65532;
- `RuntimeDefault` AppArmor and seccomp;
- no privilege escalation;
- all Linux capabilities dropped;
- read-only root filesystem;
- no host namespace or host mount;
- bounded workspace and temporary `emptyDir` volumes;
- no Service or PVC request;
- exact CPU, memory, ephemeral-storage, timeout, and output bounds;
- shutdown time and `Delete` policy; and
- one bounded base64 execution-request environment value.

The adapter adopts only a compatible existing object. It verifies identity,
security-sensitive workload shape, and shutdown bounds before reuse or
ambiguous-create cleanup. Every deletion uses an observed UID. Cleanup checks
both the known Pod and every Pod carrying the execution label.

Pod-log failures include a bounded, credential-redacted Kubernetes status body
and Pod lifecycle classification. Diagnostics distinguish missing Pods,
containers that were never created or started, waiting containers, image pull
failures, terminated containers, Kubernetes status errors, malformed bodies,
and oversized bodies.

## Helm ownership boundary

`agentSandbox.fixRuntime.enabled` defaults to `false`.

The chart can create only:

- a dashboard runtime client ServiceAccount;
- a namespace-scoped Sandbox and Pod-log Role;
- a RoleBinding in the consumer-owned execution namespace;
- a tokenless workload ServiceAccount; and
- a requester-scoped ValidatingAdmissionPolicy and binding.

Provider TLS is consumer-owned. Direct mode uses the actual HTTPS provider
operation endpoint. Gateway mode can use an internal service whose CA is in the
immutable executor image. The Fix runtime can also explicitly acknowledge a
privately resolved public gateway FQDN with `public_ca_private_dns: true`.

The chart does not install or own:

- the Agent Sandbox controller or CRD;
- the execution namespace;
- Kata, gVisor, or another RuntimeClass;
- node pools, AKS, Azure Linux, Cilium, or ACNS;
- a model gateway or provider Secret value;
- executor image publication or registry credentials; or
- consumer quotas, LimitRanges, NetworkPolicies, or monitoring.

The Role allows only Sandbox create/get/list/watch/delete, Pod
get/list/watch, and Pod-log get. Admission pins the immutable image,
RuntimeClass, AppArmor, seccomp, workload ServiceAccount, security contexts,
resources, exact request environment, emptyDir storage, scheduling restrictions,
and cleanup lifecycle.

## Evaluation history

Each result below remains preserved. Later successes do not replace earlier
failure evidence.

### 1. Fake-executor lifecycle result

Evidence: `/tmp/prow-ai-agent-sandbox-v053-evidence-20260807`

A deterministic fake executor completed one Agent Sandbox v0.5.3 lifecycle,
returned a patch, passed independent reconstruction, and cleaned up. This proved
the first adapter and controller lifecycle only. It did not run the production
OpenCode executor.

### 2. Digest-qualified local image failure

Evidence: `/tmp/prow-ai-agent-sandbox-v053-production-evidence-20260807`

The first production-executor Sandbox referenced an immutable digest, but kind
had loaded only the tag into containerd. Kubelet attempted an external pull and
the executor never started. The harness was corrected to add a local containerd
alias for the exact digest-qualified image and to run an immutable-image
preflight Pod.

### 3. AppArmor host-capability failure

Evidence: `/tmp/prow-ai-agent-sandbox-v053-production-final-20260807T223640Z`

The digest-qualified image preflight passed, but that handwritten preflight Pod
omitted the production AppArmor fields. The final Sandbox requested
`RuntimeDefault` AppArmor, while Docker Desktop kind reported that AppArmor was
not enabled on the host. The container was never created. Cleanup succeeded and
no workload remained.

This exposed two issues: the local host capability mismatch and drift between
the handwritten preflight Pod and Sandbox shape.

### 4. Corrected production-executor lifecycle result

Evidence: `/tmp/prow-ai-agent-sandbox-v053-apparmor-primary-20260807T232431Z`

The corrected evaluation used:

- Agent Sandbox v0.5.3 with the pinned release asset digest;
- the final production OpenCode executor image at local digest
  `sha256:222b65d8f4abd3a0bfecf3676c511f10e2a5da81c387860c6b422450fcc6e3c6`;
- the content-addressed containerd alias;
- the canonical local-kind workload shape with AppArmor omitted;
- Helm-generated RBAC;
- the local test-only admission policy;
- the deterministic credential-free model gateway; and
- immutable repository commit
  `7fd1a60b01f91b314f59955a4e4d4e80d8edf11d`.

The Sandbox `fix-kind-v053-production-primary-8cb680107a95` completed in
3,981 ms on
`pad-agent-sandbox-prod-20260807232431-95840-control-plane`. It changed only
`README` from `Hello World!` to `Hello Agent Sandbox!`, reported a successful
`git diff --cached --check`, passed independent patch reconstruction, and left
zero Sandbox or executor Pod resources. The cluster and temporary kubeconfigs
were deleted.

No provider credential, GitHub write token, dashboard credential, Kubernetes
credential, `Authorization` header, or API-key header entered the executor or
gateway request. No provider or GitHub write occurred.

A later independent review added the explicit public-CA private-DNS gateway
trust mode, structured terminal results for unavailable Pod logs, and a separate
budget for adapter resource metadata. The subsequent foundation review added the
structured command contract, one-shot validation semantics, toolchain contract,
and command-policy hardening. Those production-path changes required the final
evaluation below.

### 5. Final structured-command productionization result

Evidence: `/tmp/prow-ai-agent-sandbox-v053-production-foundation-20260808T005810Z`

The frozen source commit was
`560a2eec430ca6fac4bb815b75b48f964661e488`. Before the Sandbox was created, a
separate disposable cluster proved immutable-image startup, workload-shape
parity, production AppArmor API syntax, local AppArmor omission, RBAC, admission
denials, and the deterministic gateway patch cycle.

The single authorized primary Sandbox
`fix-kind-v053-production-primary-313954c33964` ran the final production
executor at local digest
`sha256:edab62dbc737dfb6cd3b353b9ba8b35a2a1cbb45d44a33cef22ae17d2877e45a`.
It checked out immutable repository commit
`7fd1a60b01f91b314f59955a4e4d4e80d8edf11d`, completed in 4,360 ms on
`pad-agent-sandbox-prod-20260808005810-20643-control-plane`, and changed only
`README` from `Hello World!` to `Hello Agent Sandbox!`.

The result reported the exact argv `git diff --cached --check`, passed
independent `git apply --check`, staged reconstruction, and diff checking, and
contained no credential-like text. The deterministic gateway received no
Authorization or API-key header and made no provider request. Cleanup removed
the Sandbox and executor Pod, then deleted the kind cluster and temporary
kubeconfigs. The frozen production-path checksum manifest remained unchanged
after the run.

## What local kind validated

The corrected local evaluation validated:

- Agent Sandbox v1beta1 creation and controller lifecycle;
- immutable image lookup and executor startup;
- canonical preflight and Sandbox workload parity;
- public immutable repository checkout;
- tokenless OpenCode gateway interaction;
- staged patch production and exact final validation command;
- structured result retrieval and independent reconstruction;
- duration and resource metadata;
- timeout and cancellation mappings through deterministic tests;
- least-privilege RBAC and fail-closed admission;
- UID-bound cleanup and orphan detection; and
- zero remaining workloads.

It did not validate:

- AppArmor enforcement;
- Kata, gVisor, or equivalent hostile-code isolation;
- AKS node pools or Azure Linux configuration;
- production egress enforcement;
- production gateway authentication or provider attachment;
- registry authentication; or
- production quotas, monitoring, and leak alerts.

Standard kind `runc` is not a hostile-code boundary.

## Remaining consumer prerequisites

A deployment still requires the consumer to provide:

1. Agent Sandbox v0.5.3 controller and CRD lifecycle;
2. a supported secure RuntimeClass and dedicated placement;
3. a node environment that supports the configured AppArmor policy;
4. an existing execution namespace;
5. the published executor image by immutable registry digest;
6. either a direct HTTPS provider or an internal HTTPS model gateway;
7. for direct bearer mode, an existing dedicated inference Secret in the execution namespace;
8. registry access configured outside the Sandbox request;
9. egress policy allowing only DNS, the public repository, and configured provider; and
10. quotas, LimitRanges, monitoring, and leaked-resource alerting.

## Readiness

The local implementation and final productionization lifecycle are ready for
split pull request review. This evaluation does not authorize merge,
image publication, Agent Sandbox deployment, AKS changes, or provider writes.
