# Kubernetes platform administrator guide

This guide prepares one cluster and one application namespace for a
prow-ai-dashboard consumer. The platform administrator owns shared prerequisites.
The project contributor owns only the reviewed consumer configuration and the
application release.

See [Kubernetes platform ownership](kubernetes-platform-ownership.md) for the
complete resource boundary.

## Required inputs

Choose one published engine tag and use its matching application and platform
chart versions:

```bash
export ENGINE_TAG="<published-engine-tag>"
export CHART_VERSION="${ENGINE_TAG#v}"
export PLATFORM_CHART_VERSION="$CHART_VERSION"
export APPLICATION_RELEASE="<application-release>"
export PLATFORM_RELEASE="${APPLICATION_RELEASE}-platform"
export NAMESPACE="<application-namespace>"
export EXECUTION_NAMESPACE="<release-dedicated-execution-namespace>"
export CONTEXT="<explicit-kubernetes-context>"
export RWX_STORAGE_CLASS="<rwx-storage-class>"
export AI_SECRET_NAME="<existing-ai-secret>"
export AI_SECRET_KEY="AI_TOKEN"
export PUBLIC_URL="<https-public-dashboard-url>"
export OAUTH_CALLBACK="${PUBLIC_URL%/}/api/auth/callback"
```

`APPLICATION_RELEASE` must be the same value used in platform values and handed
to the contributor as `RELEASE`.

The published CLI and paired chart artifacts become available with the first
release after this contract lands. Until that release exists, use only the
provider-free maintainer validation (`make cleanroom-check`) and do not substitute
a contributor-side engine build. Repeat the clean-room walkthrough with actual
release assets during release-candidate acceptance.

## 1. Provide the target Kubernetes platform

Use the team's normal infrastructure-as-code process to provide:

- a target Kubernetes cluster and explicit kube context;
- Cilium networking for the supported exact-host egress policy;
- an RWX-capable StorageClass or a reviewed existing RWX claim;
- secure-runtime nodes, labels, taints, and a real RuntimeClass handler;
- private DNS, certificates, an external edge, and public DNS where required;
- OAuth and other external identity applications.

The Helm charts do not install Kata, gVisor, a node handler, external infrastructure,
edge routing, DNS records, or OAuth applications. Do not create a placeholder
RuntimeClass whose handler is absent from the nodes.

## 2. Install the supported Agent Sandbox release

The supported upstream controller is Kubernetes SIG Agent Sandbox `v0.5.3`.
The core release artifact is `sandbox.yaml` with SHA-256:

```text
50f54b0e746376455ae6bb8b90b436bdd8798e1296cff0d72b6267bbeb858e3c
```

Pull the published platform chart, then use its verifier to obtain the upstream
artifact without executing it:

```bash
export PLATFORM_TMP="${TMPDIR:-/tmp}/prow-ai-dashboard-platform-$PLATFORM_CHART_VERSION"
install -d -m 700 "$PLATFORM_TMP"
helm pull oci://ghcr.io/willie-yao/charts/prow-ai-dashboard-platform \
  --version "$PLATFORM_CHART_VERSION" \
  --untar \
  --untardir "$PLATFORM_TMP"

"$PLATFORM_TMP/prow-ai-dashboard-platform/verify-agent-sandbox-release.sh" \
  --output "$PLATFORM_TMP/agent-sandbox-v0.5.3.yaml"
```

Review the verified manifest, then apply it as a separate cluster-admin action:

```bash
kubectl --context "$CONTEXT" apply \
  -f "$PLATFORM_TMP/agent-sandbox-v0.5.3.yaml"
```

The application and platform charts never install or upgrade the upstream CRD,
controller, webhook, or cluster RBAC.

## 3. Choose the provider contract

The standard immutable executor already contains the system public CA bundle and
the pinned Go and OpenCode toolchains. A consumer-specific executor build is not
part of the supported path.

Choose one reviewed provider path:

1. **Direct mode.** The Secret manager provisions a dedicated inference-only
   Secret directly in the execution namespace. The application references its
   name and key. No gateway image is required.
2. **Gateway mode.** The platform administrator supplies a reviewed gateway
   image digest, an existing provider Secret, an existing TLS Secret, a public
   FQDN with a publicly trusted certificate, and private DNS resolving that FQDN
   to the platform gateway Service.

Never disable TLS verification, mount arbitrary CA files into the Sandbox, copy
Secret values manually between namespaces, or reuse OAuth, bot, SMTP, GitHub,
or general-purpose credentials as the model credential.

## 4. Prepare platform values

Create a reviewed `platform-values.yaml` outside the consumer repository:

```yaml
application:
  releaseName: <application-release>

execution:
  namespace: <release-dedicated-execution-namespace>
  runtimeClassName: kata-vm-isolation
  workloadServiceAccountName: fix-workload
  networkPolicy:
    mode: cilium
    allowedFQDNs:
      - vcs.example.test
      - registry.example.test
      - artifacts.example.test
      - provider.example.test

modelGateway:
  enabled: false
```

The exact-host allowlist is project-owned and part of the security boundary.
Include only the VCS, dependency registry, artifact service, and provider hosts
required by the configured commands. Wildcards, raw CIDRs, internal names, and
unrestricted fallbacks are rejected. The current Cilium backend expects DNS in
`kube-system` with the `k8s-app: kube-dns` label.

For gateway mode, enable `modelGateway` and set only non-secret coordinates,
image digest, and existing Secret references. The values schema and chart README
show the complete contract.

## 5. Provision referenced Secrets

Use the organization Secret manager to provision required names in their owning
namespaces:

- application AI, GitHub read, OAuth, proxy, SMTP, and session Secrets in the
  application namespace;
- direct provider credentials in the execution namespace; or
- gateway provider and TLS Secrets in the application namespace.

The charts and doctor do not read, print, hash, compare, or copy Secret values.
The doctor proves only metadata existence. Required key names must be verified
through the Secret-management workflow.

## 6. Install or upgrade the platform chart

Install the platform release in the same namespace that will contain the
application release:

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

The execution namespace and binding are immutable for that platform release.
Name overrides are unsupported. Use a new platform release for a different
application or execution namespace.

## 7. Verify the platform

Give the contributor this complete non-secret handoff:

```bash
ENGINE_TAG=<published-engine-tag>
CHART_VERSION=<matching-chart-version>
RELEASE=<application-release>
NAMESPACE=<application-namespace>
EXECUTION_NAMESPACE=<release-dedicated-execution-namespace>
CONTEXT=<explicit-kubernetes-context>
RWX_STORAGE_CLASS=<rwx-storage-class-or-existing-claim>
AI_SECRET_NAME=<existing-ai-secret-or-empty-when-ai-disabled>
AI_SECRET_KEY=AI_TOKEN
PUBLIC_URL=<https-public-dashboard-url>
OAUTH_CALLBACK=<https-public-dashboard-url>/api/auth/callback
EXPECTED_JOB=<expected-job-name>
```

The contributor guide defines the doctor, install, verification, upgrade, and
rollback commands using this handoff. The doctor validates the v0.5.3 controller and CRD, RuntimeClass metadata,
Ready schedulable nodes, immutable platform binding, quota, limits, exact
NetworkPolicies, Cilium policy structure and hashes, gateway readiness, image
digests, TLS mounts, and Secret metadata.

It does not prove hostile-code isolation, real RWX behavior, provider
compatibility, certificate trust, private DNS, external edge, or OAuth application
state. Verify those facts during target-cluster release acceptance.

## 8. Upgrade, rollback, and uninstall

Use the same `helm upgrade --install` command with a reviewed newer platform
chart version. Keep the application and platform chart versions from the same
engine release.

Inspect and roll back platform history with explicit context:

```bash
helm --kube-context "$CONTEXT" --namespace "$NAMESPACE" \
  history "$PLATFORM_RELEASE"
helm --kube-context "$CONTEXT" --namespace "$NAMESPACE" \
  rollback "$PLATFORM_RELEASE" <revision> --wait
```

Uninstall retains the execution namespace, immutable binding, quota, limits,
workload identity, default-deny policy, and Cilium policy. This keeps active or
terminating Sandboxes bounded. After the application no longer references the
platform, confirm that no Sandbox, Pod, or application RBAC remains. Delete the
retained boundary only through a separate, explicitly reviewed cleanup action.
External Agent Sandbox, RuntimeClass, nodes, Secrets, DNS, certificates, and
External infrastructure remains independently owned.
