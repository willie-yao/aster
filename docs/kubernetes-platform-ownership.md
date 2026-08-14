# Kubernetes platform ownership

This contract separates one-time cluster administration from each dashboard
release. The application chart must not silently take ownership of shared or
cluster-scoped platform resources.

## Ownership matrix

| Owner | Resources and responsibilities |
| --- | --- |
| Azure infrastructure or infrastructure-as-code | AKS, node pools, secure-runtime node images, RWX storage capability, Azure Front Door, DNS, certificates, public origin restrictions, and external identity applications. |
| Kubernetes SIG Agent Sandbox release | The `sandboxes.agents.x-k8s.io` CRD, `agent-sandbox-system`, controller, webhook, and their cluster-scoped RBAC. Install the supported upstream artifact as an explicit cluster-admin operation. |
| Cluster platform administrator | A working secure `RuntimeClass`, compatible labeled nodes, the versioned platform bundle, existing Secret references, and platform upgrade and rollback. |
| Platform bundle | Agent Sandbox execution namespaces, quota, limits, tokenless workload identities, default-deny and reviewed egress policy, and an optional model-gateway Deployment and Service that reference existing Secrets. |
| Application chart | Dashboard worker or CronJob, server, application PVC, Services, optional Ingress, project ConfigMap, application ServiceAccounts, exact Sandbox admission policy, application-scoped RBAC, executor digest, and runtime bounds. |
| CAPZ consumer repository | Reviewed `project.yaml`, `prompts/system.md`, skills, `deploy/values.yaml`, provider coordinates, Secret names, public URL, and application release choices. |
| CAPZ contributor | Run the static consumer doctor and live Kubernetes doctor, perform one guarded install or upgrade, verify health and published data, and use documented Helm rollback. |
| Organization Secret manager | Provider, GitHub, OAuth, SMTP, gateway TLS, and session credential values. Consumer files and Helm arguments contain only existing Secret names and non-secret key names. |

## Required order

1. Provision AKS, secure-runtime nodes, RWX storage, and the intended public edge.
2. Install the supported upstream Agent Sandbox controller and CRD.
3. Verify the secure `RuntimeClass` and compatible Ready nodes.
4. Install the platform bundle and provision its referenced Secrets through the
   organization Secret manager.
5. Review the consumer configuration and run `fetcher onboard doctor`.
6. Run `fetcher kubernetes doctor` with an explicit kube context.
7. Run one guarded `fetcher kubernetes install` or `upgrade` command.
8. Verify application health, published CAPZ data, authentication, and the
   externally managed Front Door and DNS path.

## Application and platform boundary

The application chart intentionally does not create the Agent Sandbox
controller, CRD, execution namespace, secure runtime, compatible nodes, provider
Secret, gateway TLS Secret, or public infrastructure. It may create the exact
application client ServiceAccount, cross-namespace admission RBAC, and
release-specific admission policy because those resources depend on the
application release identity and executor contract.

The platform bundle must not create project configuration, application data
claims, application workloads, OAuth applications, credential values, or
release-specific admission policy. Each Agent Sandbox execution namespace is
dedicated to one application release and carries
`prow-ai-dashboard/release: <release>`, which gives the read-only doctor a stable
ownership boundary for active Sandboxes. A workload ServiceAccount has exactly
one owner. When the platform bundle creates it, application values set
`workloadServiceAccount.create: false` and reference the platform-owned name.

## Secret boundary

The live doctor uses Kubernetes metadata-only requests for Secret existence. It
does not fetch Secret objects, key names, values, value hashes, or Helm release
manifests. Key-name validation remains an external Secret-manager check because
Kubernetes cannot safely expose Secret key names without returning the Secret
payload.

Do not copy Secret values between namespaces as a contributor procedure. Have
the organization Secret manager provision or synchronize the required Secret
name in each owning namespace.

## Supported gateway trust

The standard immutable executor trusts the system public CA bundle. Gateway
mode therefore uses one of these contracts:

- an in-cluster HTTPS gateway whose certificate already chains to that bundle;
- a privately resolved public FQDN with a publicly trusted certificate and the
  explicit `publicCAPrivateDNS` acknowledgement; or
- another reviewed provider mode already supported by the engine.

The platform gateway Deployment declares its TLS Secret identity with the
`prow-ai-dashboard/model-gateway-tls-secret` annotation and mounts that exact
existing Secret read-only. The doctor validates the annotation, mount, and
Secret metadata without inspecting keys or values.

A consumer-specific executor that injects a private CA is a prototype
workaround, not the normal contributor contract. TLS verification must not be
disabled, and arbitrary CA mounts are not added to Sandbox workloads.

## Demo-only procedures that are not the target contract

The current CAPZ demo documents useful operational evidence, but these steps are
not normal contributor requirements:

- building a local engine binary from an exact checkout;
- using an unpublished local chart;
- applying copied upstream or consumer-specific platform YAML;
- building a consumer-specific executor or model gateway;
- injecting a private CA into the executor image;
- constructing a long raw Helm command;
- configuring Front Door with ad hoc `az afd` commands instead of the team's
  infrastructure-as-code process;
- using a temporary direct-IP kubeconfig for local DNS failure;
- manually editing owner references during a mode transition.

The direct-IP procedure preserves certificate validation and is still only a
bounded troubleshooting workaround. Disabling TLS verification, removing the
cluster CA, editing `/etc/hosts`, or using a mutable image tag is not supported.

## Verification limits

The live doctor proves observable resource presence, selected metadata,
readiness, and configuration consistency. It does not prove:

- real RWX behavior across AKS nodes;
- secure-runtime handler installation or hostile-code isolation;
- public DNS ownership, Front Door state, certificate issuance, or OAuth app
  configuration;
- model-provider compatibility or gateway authorization;
- registry provenance beyond the configured immutable reference syntax.

Those facts remain release-candidate acceptance checks in the real target
infrastructure.
