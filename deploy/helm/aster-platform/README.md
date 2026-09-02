# Aster platform chart

This chart installs the release-specific Agent Sandbox platform boundary used by one Aster application release. The cluster administrator installs it before the application chart.

For cluster prerequisites, ownership, acceptance, and the complete lifecycle, see [Kubernetes platform setup](../../../docs/kubernetes-platform.md).

## Resource ownership

The chart owns:

- one release-dedicated execution namespace;
- ResourceQuota and LimitRange bounds;
- one tokenless workload ServiceAccount;
- default-deny and exact-host Cilium egress policy;
- an immutable application, namespace, runtime, egress, and gateway binding;
- an optional digest-pinned model gateway and its Service and policies.

It does not own Agent Sandbox CRDs or controller, RuntimeClass handlers, nodes, RWX storage, Secret values, application workloads, DNS, certificates, identity applications, or external infrastructure.

Retained execution resources carry `helm.sh/resource-policy: keep` so uninstall does not remove the security boundary around active or terminating workloads.

## Required values

The chart has no default external egress grants. Supply a reviewed values file:

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

`allowedFQDNs` must list only exact project-required hosts. The current Cilium backend expects DNS in `kube-system` with `k8s-app: kube-dns`. Empty lists, wildcards, internal names, raw CIDRs, and alternate policy modes fail closed.

When `modelGateway.enabled` is true, also provide a digest-pinned image, exact upstream URL and host, existing provider and TLS Secret names, and a public FQDN for the privately resolved Service. Gateway egress is restricted to the configured upstream host.

The full schema is in `values.schema.json`.

## Agent Sandbox release

The supported upstream release is `v0.5.3`. Verify a pre-downloaded manifest:

```bash
./verify-agent-sandbox-release.sh ./sandbox.yaml
```

Or download it to a new path and verify it:

```bash
./verify-agent-sandbox-release.sh --output ./sandbox-v0.5.3.yaml
```

The script never applies or executes the artifact.

## Install or upgrade

```bash
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

Use the same command for upgrades after reviewing the render and Helm history. `application.releaseName` and `execution.namespace` cannot change after first installation.

The application values reference the platform identity:

```yaml
agentSandbox:
  fixRuntime:
    namespace: sample-sandbox
    runtimeClassName: secure-runtime
    workloadServiceAccount:
      create: false
      name: fix-workload
```

## Rollback and uninstall

Roll back with Helm to a recorded prior revision. Platform rollback does not change externally owned controller, runtime, node, storage, Secret, DNS, certificate, or provider resources.

Uninstall retains the execution namespace, binding, quota, limits, ServiceAccount, default-deny policy, and Cilium policy. Confirm there are no Sandboxes, Pods, or application RBAC references before separately deleting those retained resources.
