# prow-ai-dashboard platform chart

This chart packages cluster-platform prerequisites for one prow-ai-dashboard
application release. Install it before the application chart. ## Ownership

The chart owns:

- one release-dedicated Agent Sandbox execution namespace;
- ResourceQuota and LimitRange bounds in that namespace;
- one tokenless workload ServiceAccount;
- default-deny and reviewed egress policy;
- an optional digest-pinned model gateway, ClusterIP Service, ServiceAccount,
  and network policies.

The chart does not own:

- the Agent Sandbox CRD, controller, webhook, or cluster RBAC;
- a RuntimeClass, runtime handler, node image, labels, or taints;
- external infrastructure, RWX storage, edge routing, DNS, certificates, or OAuth;
- provider, TLS, GitHub, or application Secret values;
- application workloads, PVCs, ConfigMaps, admission policies, or
  cross-namespace application RBAC.

## Upstream Agent Sandbox contract

The supported release is Kubernetes SIG Agent Sandbox `v0.5.3`. The core
manifest is the official `sandbox.yaml` asset with SHA-256:

```text
50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c
```

Verify a pre-downloaded artifact:

```bash
./verify-agent-sandbox-release.sh ./sandbox.yaml
```

Or download to a new path and verify before a separate cluster-admin apply:

```bash
./verify-agent-sandbox-release.sh --output ./sandbox-v0.5.3.yaml
kubectl --context "$CONTEXT" apply -f ./sandbox-v0.5.3.yaml
```

The script never executes or applies downloaded content. The live Kubernetes
doctor verifies the CRD storage version, controller identity, controller image,
and readiness.

## Install or upgrade

Prepare a reviewed values file with the application release, execution
namespace, secure RuntimeClass name, and workload ServiceAccount. The platform
release is installed in the same namespace as the application so the live
doctor can discover an optional gateway through stable labels.

```bash
helm upgrade --install "$PLATFORM_RELEASE" \
  oci://ghcr.io/willie-yao/charts/prow-ai-dashboard-platform \
  --version "$PLATFORM_CHART_VERSION" \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --kube-context "$CONTEXT" \
  --values platform-values.yaml \
  --wait \
  --rollback-on-failure
```

Use the same command for upgrades. Review the render and current release history
first. Helm rollback does not remove retained application data or external
Secrets.

The application values reference the platform-owned identity:

```yaml
agentSandbox:
  fixRuntime:
    namespace: <execution-namespace>
    runtimeClassName: <secure-runtime-class>
    workloadServiceAccount:
      create: false
      name: <platform-workload-service-account>
```

The application chart continues to own its client ServiceAccount,
cross-namespace Role and RoleBinding, and exact release-specific admission
policy.

## Network policy

The currently supported FQDN-aware network-policy contract requires
`execution.networkPolicy.mode: cilium` so hostile workloads use reviewed
exact-host FQDN policy. Wildcards, public suffixes, multi-tenant wildcard domains, internal names, raw CIDRs, and a
portable unrestricted fallback are not supported.

`execution.networkPolicy.allowedFQDNs` has no default grants and must contain an
explicit reviewed list. Include only the VCS, dependency registry, artifact
service, and provider hosts required by the project commands. An empty list
fails closed. The current backend selects DNS in `kube-system` with the
`k8s-app: kube-dns` label. Clusters with a different DNS identity are not yet
supported by this platform chart.

A normal kind cluster can validate chart lifecycle only with a disposable
test-only Cilium CRD. It does not prove Cilium behavior, secure RuntimeClass
enforcement, hostile-code isolation, or target-cluster networking.

## Model gateway and TLS

Gateway mode is optional. Direct provider modes supported by the application do
not require this Deployment. When enabled, the platform administrator supplies:

- a reviewed gateway image pinned by OCI digest;
- an existing provider Secret name and key;
- an existing TLS Secret name;
- an HTTPS provider upstream and exact upstream host;
- a public FQDN whose private DNS record resolves to the gateway Service.

Gateway egress is always limited to the configured `upstreamHost`. The chart
rejects a separate additional-host allowlist so the rendered policy matches the
doctor's immutable gateway binding.

The gateway Deployment carries stable host and TLS-Secret annotations. It mounts
the exact TLS Secret read-only in the `gateway` container. The chart never
renders Secret values. Certificate issuance, SANs, private keys, renewal, and
private DNS remain externally owned. The doctor reports the handshake and trust
chain as unverified until real release acceptance.

## Uninstall

The execution Namespace, immutable binding, quota, limits, tokenless workload
identity, default-deny policy, and Cilium egress policy all have Helm's keep
policy. Uninstall therefore retains the complete execution security boundary for
explicit, separately confirmed cleanup, including protection for active or
terminating Sandboxes. Confirm that no Sandbox, Pod, or application RBAC remains
before deleting those resources and then the namespace. External Agent Sandbox,
RuntimeClass, node, Secret, DNS, certificate, and external infrastructure are retained.

`application.releaseName` and `execution.namespace` are immutable after the
first installation. Use a new platform release for a different binding rather
than renaming either value during upgrade.
