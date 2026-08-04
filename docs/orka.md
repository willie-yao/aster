# Orka integration

Orka is optional. The default Kubernetes dashboard runs analysis in-process and
does not need Orka. Install and configure Orka only when a concrete requirement
justifies its separate controller, CRDs, Task lifecycle, and credential model.

The dashboard chart does not install Orka and does not list it as a dependency.
Orka is a separate cluster-level release that may be shared by multiple
dashboard releases.

Orka can support three independent integrations:

- Experimental containerized failure analysis.
- Read-only source investigation from analysis chat.
- Agent-proposed fix generation.

Enabling one integration does not enable the others. See
[Orka architecture](orka-architecture.md) for the component and ownership model.

## Installation availability

This repository does not configure a verified published Orka chart and runtime
release. The general cloud-cluster install path is therefore blocked until an
operator records immutable release metadata.

The maintained consumer example is the
[CAPZ Prow AI Dashboard Orka Demo](https://github.com/willie-yao/capz-prow-ai-dashboard-orka-demo/tree/main/deploy/orka).
Its installer refuses to run while these release fields are missing:

```text
ORKA_CHART_REFERENCE
ORKA_CHART_VERSION
ORKA_CHART_DIGEST
ORKA_CONTROLLER_DIGEST
ORKA_AI_WORKER_DIGEST
ORKA_GENERAL_WORKER_DIGEST
ORKA_HARNESS_WRAPPER_DIGEST
```

Do not invent a chart URL, publish an unofficial release, or substitute mutable
`main` or `latest` image tags. The chart source version alone is not evidence of
a published release.

The minimum source revision currently required by the dashboard integration is:

```text
fde3b7925c367784570fcc36d7a5b3a51747bf10
```

A usable published release must contain that revision or a later compatible
revision and publish:

- The Helm chart.
- Controller image.
- AI worker image.
- General worker image.
- Agent harness-wrapper image.

Pin the exact chart version, chart package digest, and all four image digests.
A digest for only the controller is not sufficient.

## Maintained installation path

After verified release metadata is recorded in the consumer repository, use its
installer with an explicit context:

```bash
export CONTEXT="<explicit-kubernetes-context>"
export ORKA_RELEASE="orka"
export ORKA_NAMESPACE="orka-system"

./deploy/orka/install.sh \
  --context "$CONTEXT" \
  --release "$ORKA_RELEASE" \
  --namespace "$ORKA_NAMESPACE"
```

At the current unconfigured release state, this command is expected to refuse
before changing the cluster. That refusal is a safety feature.

The installer must:

1. Require an explicit context, release, and namespace.
2. Download the exact chart package.
3. Verify its SHA-256 digest before Helm reads it.
4. Render every runtime image as `tag@sha256:digest`.
5. Refuse an existing Orka release during a fresh install.
6. Install and wait for the release.
7. Validate CRDs, controller, services, storage, RBAC, and running image digests.
8. Record non-secret version evidence.

The dashboard install remains separate and must never invoke Orka installation
implicitly.

## Source-only validation

Packaging a chart from an exact source commit is a maintainer-only,
non-production path. It can support lint, render, and disposable kind testing.
It does not provide matching released runtime images and must not be used as a
cloud-cluster installation shortcut.

See `experimental/orka/README.md` and
`experimental/orka/run-container-analyzer-kind.sh` for the disposable engine
integration test. Do not adapt that path into production installation guidance.

## Fresh install and readiness

A fresh compatible chart install creates 12 cluster-scoped Orka CRDs from the
chart `crds/` directory before templated release resources. It also manages:

- The controller, Service, and controller RBAC.
- Worker ServiceAccounts and RBAC.
- Harness-wrapper Deployment and Service.
- Release-local authentication state.
- The persistent result store.

After installation:

1. Wait for every CRD to become `Established`.
2. Verify controller and harness-wrapper readiness.
3. Verify the REST Service and store PVC.
4. Verify controller permissions for required resource kinds.
5. Verify worker identities.
6. Verify rendered and running images match all pinned digests.

Do not broaden a dashboard ServiceAccount to compensate for an Orka controller
installation error.

The Orka chart does not create model credentials or a project Agent. Create the
Agent model Secret separately, apply a reviewed Agent with the endpoint-specific
model ID, and wait for its `Ready` condition.

For an OpenCode Agent:

- Set `spec.runtime.type: opencode`.
- Put `OPENAI_BASE_URL` in the Agent model Secret.
- Add `OPENAI_API_KEY` only when the endpoint requires authentication.
- Keep any private-repository clone credential separate and read-only.
- Never give the Agent a dashboard GitHub write token.

See `configs/example/orka-opencode-agent.yaml` for the manifest shape. Do not
apply it without replacing and reviewing all placeholders.

## CRD-first upgrades

Helm installs files under `crds/` during a fresh install but does not upgrade
those CRDs. Upgrade the CRDs before the controller and stop every Task-producing
client until validation completes.

A safe upgrade is:

1. Acquire the cluster-wide `orka-crd-lifecycle` Lease.
2. Download or locate the exact target chart package.
3. Verify the chart package digest.
4. Read the exact target CRD inventory with `helm show crds`.
5. For each current CRD, read its `resourceVersion`.
6. Use a JSON Patch that tests that version before replacing the complete target
   `spec`.
7. Wait for all 12 CRDs to become `Established`.
8. Run the Helm upgrade with all runtime image digests pinned.
9. Wait for the controller and harness wrapper.
10. Run the full release validation before restarting Task producers.

The exact-spec replacement removes fields deleted by the target schema without
deleting custom resources. A plain `kubectl apply` can retain removed schema
fields and is not an equivalent upgrade.

Use the maintained consumer `deploy/orka/upgrade.sh` when verified release
metadata is available. It serializes the lifecycle and records a pre-upgrade
resource inventory.

## Uninstall and release topology

`helm uninstall` removes release-scoped resources, including a chart-managed
store PVC, but retains CRDs installed from `crds/`. Orka custom resources also
remain in the Kubernetes API.

Back up result-store data before uninstall when Task results or sessions must
survive. Deleting a CRD deletes every custom resource of that kind across the
cluster. Treat CRD deletion as a separate destructive operation and never make
it part of dashboard uninstall.

One cluster-wide Orka release may serve multiple dashboards. If multiple Orka
releases are required, each needs:

- A unique release name or `fullnameOverride`.
- An isolated controller namespace.
- A distinct non-empty `controller.watchNamespace`.

Do not combine a cluster-wide watcher with namespace-scoped releases whose
reconciliation or admission scopes overlap.

## Default in-process analysis

`analysisRuntime.type: inprocess` is the recommended default. It works in Pages
and in Kubernetes watch or cron mode. It keeps prompts, tools, evidence policy,
critique, semantic review, cache acceptance, traces, and result schemas inside
the dashboard implementation.

Use this mode unless Orka Task isolation or Task retry history is a real
operational requirement.

## Experimental Orka container analysis

Set `analysisRuntime.type: orka-container` to submit one content-addressed Orka
container Task per failure. This is an experimental Helm-only lifecycle
sidegrade for watch or cron mode. It has no backward compatibility guarantee
and is not recommended over in-process analysis.

The analyzer image still runs the dashboard `FailureAnalyzer`. Orka owns Task
and Job lifecycle, retries, timeout, and durable result transport. It does not
own prompts, evidence selection, tools, critique, cache acceptance, or final
result schemas.

Example values after Orka and all required Secrets are installed:

```yaml
analysisRuntime:
  type: orka-container
  orkaContainer:
    namespace: ""
    api: http://orka.orka-system.svc.cluster.local:8080
    apiAuth:
      existingSecret: ""
      tokenKey: token
    maxConcurrentTasks: 2
    pollInterval: 2s
    taskTimeout: 20m
    retries: 1
    image:
      repository: ghcr.io/willie-yao/prow-ai-dashboard/analyzer
      tag: "<immutable-engine-version>"
      pullPolicy: IfNotPresent
    modelAuth:
      existingSecret: "<model-secret-in-analysis-namespace>"
      tokenKey: token
    state:
      existingSecret: ""
      key: state-key
    nodeSelector:
      agentpool: "<cpu-agentpool>"
    tolerations: []
    affinity: {}
```

This configuration does not install Orka. `orkaContainer.api` must name the REST
Service of the separately installed release. The Service name is not derived
from its namespace.

### Result API authentication

With an empty `apiAuth.existingSecret`, the dashboard uses a projected rotating
ServiceAccount token and reloads it for each result request. Use a static Secret
only when the Orka API cannot accept that ServiceAccount identity.

Result API authentication is separate from the model token stored in the
analysis namespace.

### Timeouts and concurrency

`taskTimeout` must be at least the project `ai.timeout` plus two minutes for Task
startup and encrypted result finalization. The worker rejects a shorter outer
timeout instead of allowing Orka to terminate the analyzer before recoverable
state is emitted.

Watch passes never overlap. A long Task wave delays the next refresh. Do not
create a manual fetch Job while the watch worker exists.

### Task identity and reuse

Before creating analyzer Tasks, the worker applies private cache entries that
pass current identity, age, quality, critique, and malformed-state checks.
Subjects satisfied from private cache still count toward logical work, but do
not create Tasks.

If private cache misses, planning checks the exact content-addressed Task.
Exact reuse requires:

- A non-deleting succeeded managed Task.
- A durable result reference.
- The exact bundle digest and state-key fingerprint.
- The current analyzer contract.
- Authenticated encrypted state.
- Agreement between the encrypted cache entry and public result.
- Current investigation and critique gates.

If exact reuse misses, the worker can inspect a bounded set of recent succeeded
Tasks for the same work item. Compatible reuse preserves the result,
authentication, state, and quality contracts while allowing a previous Task
identity.

Task adoption does not relabel an existing Task. Private fetch-status state
retains the current-pass correlation.

### State Secrets

When `state.existingSecret` is empty, the chart creates retained matching
release-scoped state-key Secrets in the dashboard and analysis namespaces.

When providing an external state Secret, create the same Secret name and key in
both namespaces. Generate one shared random literal through an approved Secret
management path. Do not print the key or commit it.

The state key protects transported private cache and trace state. The model
Secret remains separate.

### Analysis namespace and admission

When `orkaContainer.namespace` is empty, the chart creates and retains a
namespace dedicated to the dashboard release. A custom namespace must satisfy
the chart release-scope rule. It must not be the Orka controller namespace, fix
runtime namespace, or dashboard namespace.

Keep only analyzer model and state Secrets in the analysis namespace.

Container analysis installs a fail-closed `ValidatingAdmissionPolicy` that pins:

- Analyzer image and arguments.
- Model coordinates.
- CPU placement.
- Bundle reference.
- Exact model and state Secret references.

The installer therefore needs permission to create cluster-scoped admission
policies.

The immutable input ConfigMap contains sanitized project policy, prompt, skills,
request data, and a bounded raw cache seed. It never contains model credentials.
Projects using custom `ai.headers` are rejected because the adapter has no secure
cross-namespace transport for those values. Use bearer-token authentication or
a trusted proxy.

Analyzer Tasks must run on CPU nodes. The chart requires an explicit CPU
`agentpool` selector and rejects accelerator selectors, affinity, and
tolerations that could place the analyzer on accelerator nodes. Run the Orka
controller and helper workloads on CPU nodes as well. Only the model-serving
workload should select GPU nodes.

## Read-only source investigation

Source investigation is independent from failure-analysis runtime selection. It
uses an Orka Agent Task to inspect source for an authenticated chat session.
Enable it only after the Orka release, Agent, read-only repository credential,
and Task-only RBAC are ready.

Source investigation requires authenticated analysis chat and the Helm-side
source-investigation controller:

```yaml
server:
  chat:
    enabled: true
    sourceInvestigation:
      enabled: true
      serviceAccountName: ""
      admission:
        agentRef: "<guarded-read-only-agent>"
        repository:
          owner: "<github-owner>"
          name: "<github-repository>"
        gitSecret: "<read-only-clone-secret>"
        maxTurns: 30
        timeout: 10m
        retries: 1
  actions:
    enabled: false
    mode: oauth
    admins:
      - "<github-login>"
    oauth:
      clientId: "<oauth-client-id>"
      redirectUrl: "https://dashboard.example.com/api/auth/callback"
      existingSecret: "<oauth-secret>"
```

Configure a secure origin before enabling authenticated chat. See
[Server mode](server.md) for OAuth, proxy authentication, admin allowlists,
NetworkPolicy, and origin requirements.

Project configuration shape:

```yaml
ai:
  source_investigation:
    agent_ref: "<guarded-read-only-agent>"
    api: http://orka.orka-system.svc.cluster.local:8080
    namespace: orka-system
    git_secret: "<read-only-clone-secret>"
    max_turns: 30
    timeout: 10m
    retries: 1
```

The Agent runtime must satisfy Orka's enforced read-only contract. Do not remove
the guard to make an unsupported runtime start. The clone Secret must contain
only read-only repository credentials.

The dashboard server uses a dedicated ServiceAccount with Task create, get,
patch, and delete permissions. A requester-scoped `ValidatingAdmissionPolicy`
pins the Agent, repository, immutable revision shape, exact read-only Git Secret,
read-only tool list, timeout, retries, and Task metadata. It rejects images,
commands, environment variables, alternate Secrets, Bash, write or network
tools, scheduling, webhooks, sessions, and placement overrides. The projected
ServiceAccount token is mounted only because the server must create and cancel
Tasks and read the authenticated Orka result API.

The server independently verifies returned source quotes against the pinned
commit. Private source repositories also require a separate read-only GitHub
token Secret for that verification. The Helm admission values intentionally
duplicate the security-sensitive `project.yaml` settings; a mismatch denies the
Task.

Source investigation does not require the git-capable fixer image and does not
enable write actions.

## Orka fix generation

Orka fix generation is independent from container analysis. Set:

```yaml
orka:
  fixRuntime:
    enabled: true
    admission:
      agentRef: "<orka-agent-name>"
      repository:
        owner: "<github-owner>"
        name: "<github-repository>"
      maxTurns: 30
      allowBash: true
      timeout: 15m
      retries: 1
```

Then configure the consumer project with the required Agent and result API
coordinates:

```yaml
ai:
  fix_prs:
    enabled: true
    agent_runtime:
      type: orka
      agent_ref: "<orka-agent-name>"
      api: http://orka.orka-system.svc.cluster.local:8080
      namespace: orka-system
      version: v1
      retries: 1
      max_turns: 30
      allow_bash: true
      timeout: 15m
```

This selects the git-capable fixer image, a separate Task-only Role, and a
fail-closed `ValidatingAdmissionPolicy`. The Helm admission values must match the
effective `project.yaml` values. A mismatch denies Task creation. Enabling
container analysis does not enable or configure fix generation.

The dashboard and Agent runtime settings are separate:

- `ai.fix_prs.agent_runtime.type: orka` selects Orka as the generation backend.
- `Agent.spec.runtime.type: opencode` selects OpenCode inside Orka.

The operator owns the Agent and model Secret. Put endpoint and model credentials
in the Agent Secret, not `project.yaml`. Guarded fix Tasks reject workspace Git
Secrets, so the repository must currently be publicly cloneable. `FIX_TOKEN`
stays in the dashboard workload and is never passed to the Agent, Task,
workspace, or model Secret.

See [Agent-proposed fix PRs](fix-prs.md#orka-in-cluster) for project settings and
identity boundaries.

The policy is scoped by the authenticated dashboard ServiceAccount and the Orka
namespace. It pins the exact Agent namespace, repository, immutable commit
shape, generation-only workspace, turn and Bash limits, timeout, retry policy,
priority, Agent-owned resources, and dashboard metadata. It rejects container
fields, Task and workspace Secret references, custom environment variables,
scheduling, sessions, webhooks, prior Tasks, mutable Git refs, tool overrides,
and placement overrides. Unrelated Orka requesters are not matched. Container
analyzer Tasks remain governed by the existing analyzer policy in their
dedicated namespace.

A dedicated fix Task namespace is not safe with the current Orka contract. Orka
can enforce same-namespace Agent references, and the Agent's namespaced Secret
cannot be mounted into a worker Job in another namespace without copying the
credential. Keep fix Tasks with the approved Agent in `orka.namespace` until
Orka provides a brokered credential and cross-namespace Agent contract.

Source investigation and fix actions also cannot share one server pod today.
They would use one Kubernetes requester for two different Task shapes, which
would either deny source Tasks or leave fix Tasks outside the strict policy. The
chart rejects that combined mode until the requesters are separated.

When the dashboard namespace differs from `orka.namespace`, grant the dashboard
ServiceAccount access to the Orka result API. Prefer projected ServiceAccount
authentication. Use a static API token only when namespace policy cannot accept
that identity.

## Operational checklist

Before enabling any Orka integration:

1. Confirm a verified immutable Orka release is configured.
2. Confirm all 12 CRDs are established.
3. Confirm controller, wrapper, storage, and services are ready.
4. Confirm rendered and running image digests.
5. Confirm the model Secret exists only in the intended runtime namespace.
6. Confirm dashboard ServiceAccounts have Task-only permissions.
7. Confirm analysis and helper workloads select CPU nodes.
8. Confirm no active Tasks before an Orka upgrade.
9. Enable only one integration at a time and validate its result path.
10. Keep the dashboard installation and Orka lifecycle separate.

## Related references

- [Kubernetes quickstart](kubernetes.md)
- [Kubernetes operator reference](kubernetes-reference.md)
- [Orka architecture](orka-architecture.md)
- [Analysis runtime evaluation](analysis-runtime-evaluation.md)
- [Agent-proposed fix PRs](fix-prs.md)
- `experimental/orka/README.md`
