# Kubernetes platform setup

This guide defines the cluster-administrator contract for one or more
Aster consumer releases. Project contributors own reviewed consumer
configuration and application releases. Platform administrators own shared or
cluster-scoped prerequisites.

The platform chart ships with the application release but has an independent
Helm release and resource identity. It does not make cloud-provider API calls.
Provider-specific edge, DNS, storage, node, and identity configuration remains
in infrastructure-as-code or the consumer's infrastructure repository.

## Ownership matrix

| Owner | Resources and responsibilities |
| --- | --- |
| External infrastructure | Target Kubernetes cluster, node pools, secure-runtime node images, RWX storage capability, external edge, DNS, certificates, and identity applications. |
| Agent Sandbox upstream release | Sandbox CRD, controller, webhook, namespace, and cluster RBAC for the supported version. |
| Platform administrator | Secure RuntimeClass, compatible nodes, platform chart, reviewed egress, existing Secret references, and target-cluster acceptance. |
| Platform chart | Release-dedicated execution namespace, quota, limits, tokenless workload identity, default-deny and Cilium egress policy, immutable binding, and optional model gateway. |
| Application chart | Dashboard writer and server, application PVC, Services, optional Ingress, consumer ConfigMap, application identities, release-specific Sandbox admission, and application RBAC. |
| Consumer repository | `project.yaml`, prompt, skills, application values, provider coordinates, Secret names, public origin, and expected project jobs. |
| Secret manager | Provider, repository, OAuth, SMTP, gateway TLS, and session credential values. |

The application chart must not claim cluster-scoped prerequisites. The platform
chart must not create application data, consumer configuration, external
infrastructure, credential values, or release-specific application policy.

## Existing externally managed platforms

The live doctor has an upgrade-only compatibility mode for an existing
application release with no platform binding and no partial platform-chart
ownership metadata. It reports `platform ownership: externally managed` and
validates the observable controller, namespace, RuntimeClass, Ready nodes,
quota, limits, tokenless ServiceAccount, default-deny and exact-host egress,
gateway, TLS reference, and absence of active Sandboxes.

A missing or weak prerequisite still blocks the upgrade. Any binding,
platform-release annotation, application-release label, or platform ownership
marker selects strict chart-managed validation. New installs that enable Agent
Sandbox Fix must use the platform chart.

## Required platform inputs

Choose one published release and prepare:

```bash
export ENGINE_TAG="<published-engine-tag>"
export PLATFORM_CHART_VERSION="${ENGINE_TAG#v}"
export APPLICATION_RELEASE="<application-release>"
export PLATFORM_RELEASE="${APPLICATION_RELEASE}-platform"
export NAMESPACE="<application-namespace>"
export EXECUTION_NAMESPACE="<execution-namespace>"
export CONTEXT="<explicit-kubernetes-context>"
export RUNTIME_CLASS="<secure-runtime-class>"
```

Also provide an RWX StorageClass or existing claim, compatible Ready nodes,
existing Secret names, provider coordinates, public origin, and project-owned
network egress requirements.

## Install Agent Sandbox

The supported release is Kubernetes SIG Agent Sandbox `v0.5.3`. The platform
chart includes a verifier for the official `sandbox.yaml` asset and its pinned
SHA-256.

```bash
helm pull oci://ghcr.io/willie-yao/charts/aster-platform \
  --version "$PLATFORM_CHART_VERSION" --untar
./aster-platform/verify-agent-sandbox-release.sh \
  --output ./sandbox-v0.5.3.yaml
kubectl --context "$CONTEXT" apply -f ./sandbox-v0.5.3.yaml
```

The verifier downloads and validates the artifact but never executes or applies
it. Installation of the verified upstream manifest is a separate cluster-admin
operation. The live doctor checks the CRD storage version, controller identity,
controller image, endpoints, and readiness.

## RuntimeClass, nodes, and storage

### Secure-runtime contract

Aster's core dashboard, in-process read-only analysis, and ordinary GitOps
deployment do not inherently require Kata or any Agent Sandbox RuntimeClass. A
secure runtime is required only when a configured feature creates Agent Sandbox
workloads. Current examples are Agent Sandbox Fix execution and the optional
Agent Sandbox analyzer and causal critic experiments. When those features are
disabled, Aster can run without Agent Sandbox or a sandbox RuntimeClass.

Aster accepts the configured `runtimeClassName`; it does not hardcode Kata. The
platform administrator must install and review the RuntimeClass handler, provide
compatible nodes, and complete target-cluster isolation acceptance. For
configured Agent Sandbox paths, Aster validates the RuntimeClass identity,
scheduling compatibility with Ready nodes, workload shape, and admission
contract. These checks do not prove that the handler provides VM or sandbox
isolation.

Agent Sandbox Fix can execute repository-controlled validation commands.
RuntimeClass presence, node labels, taints, and a dedicated node pool can
constrain scheduling, but are not themselves a hostile-code runtime boundary.
Standard `runc` is not the intended production boundary for repository code
execution.

The application and platform charts do not create a RuntimeClass handler,
install runtime-capable node images, or configure cloud infrastructure. Provider
setup remains owned by the Kubernetes or cloud platform administrator.

Examples only. These are not automatic compatibility guarantees.

| Provider or environment | Example secure runtime |
| --- | --- |
| AKS | Kata or AKS Pod Sandboxing |
| GKE | gVisor or GKE Sandbox |
| EKS | A separately validated sandbox or microVM execution path |
| Self-managed Kubernetes | Kata, gVisor, or equivalent |

Provide RWX storage for application data. Metadata can prove that a claim is
Bound and declares `ReadWriteMany`; it cannot prove working multi-node RWX
semantics. Test real writer and server access during target-cluster acceptance.

## Network-policy backend

The platform chart currently supports Cilium as its FQDN-aware backend. It does
not offer a portable fallback with weaker external-host isolation.

`execution.networkPolicy.allowedFQDNs` is required and has no default grants.
Supply only exact VCS, dependency registry, artifact service, and provider hosts
needed by the project's configured commands. Wildcards, internal names, raw
CIDRs, and unrestricted public egress are rejected.

The current backend selects DNS in `kube-system` with the `k8s-app: kube-dns`
label. A cluster with a different DNS identity is not supported by this chart
version. Kind tests validate chart lifecycle only and do not prove Cilium data
plane behavior.

## Model gateway and TLS

Gateway mode is optional. Direct provider modes do not require the platform
model-gateway Deployment. When gateway mode is enabled, supply:

- a reviewed gateway image pinned by OCI digest;
- an existing provider Secret name and key;
- an existing TLS Secret name;
- an HTTPS upstream URL and exact upstream host;
- a public FQDN whose private DNS resolves to the gateway Service.

Gateway egress is restricted to the configured upstream host. The chart rejects
additional gateway egress hosts. Certificate issuance, SANs, private keys,
renewal, private DNS, provider authorization, and public trust remain externally
owned.

## Secret ownership

Provision credential values with the organization Secret manager. Consumer and
platform values contain only existing Secret names and non-secret key names.
Do not copy Secret values between namespaces as a contributor procedure.

The live doctor performs metadata-only Secret existence requests. It never
reads payloads, key names, values, or hashes. Verify key names and credential
scope in the Secret manager.

## Install or upgrade the platform chart

Create a reviewed `platform-values.yaml` outside the consumer repository:

```yaml
application:
  releaseName: sample-dashboard
execution:
  namespace: sample-sandbox
  runtimeClassName: secure-runtime
  workloadServiceAccountName: fix-workload
  networkPolicy:
    mode: cilium
    allowedFQDNs:
      - vcs.example.test
      - registry.example.test
      - artifacts.example.test
modelGateway:
  enabled: false
```

Render and install with the explicit context:

```bash
helm template "$PLATFORM_RELEASE" \
  oci://ghcr.io/willie-yao/charts/aster-platform \
  --version "$PLATFORM_CHART_VERSION" \
  --namespace "$NAMESPACE" \
  --values platform-values.yaml >/dev/null
helm upgrade --install "$PLATFORM_RELEASE" \
  oci://ghcr.io/willie-yao/charts/aster-platform \
  --version "$PLATFORM_CHART_VERSION" \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --kube-context "$CONTEXT" \
  --values platform-values.yaml \
  --wait \
  --rollback-on-failure
```

`application.releaseName` and `execution.namespace` are immutable after first
installation. Use a new platform release for a different binding.

Hand these non-secret values to the project contributor:

- application release, namespace, execution namespace, and explicit context;
- engine tag and paired chart versions;
- RWX storage coordinates;
- existing Secret names and non-secret key names;
- public origin, OAuth callback, and expected project job.

## Verify target-cluster acceptance

Run `aster kubernetes doctor` before the application install. Resolve all
blocking checks. The doctor cannot prove:

- secure-runtime isolation or node-handler functionality;
- real multi-node RWX behavior;
- Cilium enforcement against hostile workloads;
- provider or gateway compatibility;
- certificate trust, external edge, DNS ownership, or OAuth state.

Validate those facts on the target cluster without disabling TLS verification,
using direct-IP kubeconfigs, or introducing mutable images.

## Upgrade, rollback, and uninstall

Review the rendered diff and Helm history before a platform upgrade. Use the
same install command with the new paired chart version. If acceptance fails:

```bash
helm --kube-context "$CONTEXT" -n "$NAMESPACE" \
  rollback "$PLATFORM_RELEASE" <prior-revision> --wait
```

Platform uninstall intentionally retains the execution namespace, immutable
binding, quota, limits, tokenless ServiceAccount, default-deny policy, and
Cilium policy. Confirm that no Sandbox, Pod, or application RBAC remains before
separately deleting retained resources. Agent Sandbox, RuntimeClass, nodes,
Secrets, storage, DNS, certificates, and external infrastructure remain owned by
their original systems.

See the [platform chart README](../deploy/helm/aster-platform/README.md)
for exact values and retention details, and the
[Kubernetes operator reference](kubernetes-reference.md) for application
architecture and advanced behavior.
