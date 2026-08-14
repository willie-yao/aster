# Flux GitOps deployment

Aster can generate a provider-free Flux bundle for a pull-based Kubernetes
deployment. The bundle contains only standard Flux source and Helm release
resources. It does not install Flux, create repository credentials, provision
Azure infrastructure, create runtime Secret values, or contact a cluster.

Use this workflow after the application and platform charts are published at
one reviewed semantic version. Direct Helm install and upgrade remain supported.

## Contributor workflow

Keep the reviewed consumer inputs in Git:

```text
project.yaml
prompts/system.md
skills/*.yaml
deploy/values.yaml
deploy/platform-values.yaml  # required only when Agent Sandbox Fix is enabled
```

Set the release coordinates from the intended deployment:

```bash
export ASTER_VERSION="0.9.0"
export RELEASE="<application-release>"
export NAMESPACE="<application-namespace>"
export EXECUTION_NAMESPACE="<execution-namespace>" # Fix-enabled deployments only
```

Generate the declaration:

```bash
aster kubernetes gitops render \
  --project-dir . \
  --values deploy/values.yaml \
  --platform-values deploy/platform-values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --execution-namespace "$EXECUTION_NAMESPACE" \
  --chart-version "$ASTER_VERSION" \
  --output gitops
```

Review every changed source input and generated file. Then verify that the
checked-in declaration is current:

```bash
aster kubernetes gitops check \
  --project-dir . \
  --values deploy/values.yaml \
  --platform-values deploy/platform-values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --execution-namespace "$EXECUTION_NAMESPACE" \
  --chart-version "$ASTER_VERSION" \
  --gitops-dir gitops
```

For an application-only deployment, omit `--execution-namespace`. The platform
values file is not read and the generator omits the platform source and release.

`render` and `check` are local operations. They do not read a kubeconfig, call a
provider, contact an OCI registry, or inspect a Kubernetes cluster. `render`
supports `--dry-run` to print the bounded create, replace, and remove plan
without writing files.

## Generated contract

The bundle uses:

- `source.toolkit.fluxcd.io/v1` `OCIRepository` objects for the application and,
  when required, platform charts;
- `helm.toolkit.fluxcd.io/v2` `HelmRelease` objects;
- ordinary `kustomize.config.k8s.io/v1beta1` `kustomization.yaml` files;
- values ConfigMaps labeled `reconcile.fluxcd.io/watch: Enabled`;
- exact semantic-version chart tags, with application and platform pinned to
  the same version;
- release tags that the publishing registry treats as immutable, because Flux
  detects digest replacement behind an unchanged tag;
- an application `dependsOn` reference to the platform HelmRelease when Agent
  Sandbox Fix is enabled;
- Helm install and upgrade remediation with readiness waiting and rollback on a
  failed upgrade.

The generated application `values.yaml` is the canonical Helm input used by
Flux. It contains the reviewed `deploy/values.yaml` plus inline project files:

```yaml
project:
  config: |
    <project.yaml>
  systemPrompt: |
    <prompts/system.md>
  skills:
    <stable filename>: |
      <skill content>
```

A Fix-enabled application continues to use inline project values. The generator
does not switch it to `project.existingConfigMap`, because the application chart
must compare security-sensitive project and runtime settings.

The ConfigMaps contain operational project configuration and prompt text. They
contain no Secret values, but the consumer Git repository should still be
access-controlled. Runtime credentials, TLS private keys, OAuth client secrets,
provider tokens, session keys, and Flux repository credentials must be
provisioned separately as Kubernetes Secrets. Generated values may reference
existing Secret names and non-secret key names.

`check` regenerates the expected files without writing them. It compares all
generated files byte-for-byte and reports only missing, stale, or unexpected
paths. It also rejects mutable versions, Secret manifests, unsafe paths,
credential material, broken ConfigMap references, and invalid dependency or
version pairing.

## Provider-free CI

A generic validation workflow is available at
[`examples/aster-gitops-check.yml`](examples/aster-gitops-check.yml). Copy it to
`.github/workflows/aster-gitops-check.yml` in the consumer repository and set the
pinned CLI version, binary checksum, release, namespace, execution namespace,
and chart version.

The workflow downloads one pinned Aster CLI asset, verifies its checksum, runs
the static consumer doctor and GitOps check, renders the application and
optional platform charts, and scans the generated directory for obvious
credential material. It requires no AKS credentials and performs no deployment.

## One-time repository connection

After Aster and its paired charts are published, a platform administrator:

1. Creates or selects the consumer Git repository.
2. Installs a supported `microsoft.flux` extension on the AKS cluster.
3. Configures one Flux Git source for the reviewed branch and `./gitops` path.
4. Uses a read-only deploy key or reviewed HTTPS credential for a private
   repository. The credential is not stored in the generated bundle.
5. Provisions Aster runtime Secrets separately.
6. Merges the initial generated bundle.
7. Runs the Aster live doctor with the explicit cluster context.
8. Verifies HelmRelease readiness and the public endpoint.

See the official
[AKS and Azure Arc Flux v2 tutorial](https://learn.microsoft.com/en-us/azure/azure-arc/kubernetes/tutorial-use-gitops-flux2)
and [Flux extension release notes](https://learn.microsoft.com/en-us/azure/azure-arc/kubernetes/flux-gitops-release-notes).
Use a Flux extension version that supports the stable APIs generated by Aster.

Flux manages the Kubernetes resources declared by the chart releases. It does
not provision AKS, node pools, storage infrastructure, Front Door, DNS,
certificates, OAuth applications, Key Vault, or other Azure infrastructure.

## Rollback and incident response

The normal rollback is a Git operation:

```text
revert the deployment commit
-> merge the revert
-> Flux reconciles the previous chart and values
```

A revert does not delete retained application PVC data. It also does not delete
platform resources retained by the platform chart or infrastructure owned
outside Helm.

During incident response, an operator may suspend the affected HelmRelease or
the enclosing Flux reconciliation before making a bounded manual intervention.
Inspect `OCIRepository` and `HelmRelease` Ready conditions, Helm history, and
controller events without reading Secret values. Restore the reviewed Git state,
then resume reconciliation and confirm that both releases become Ready. Do not
add a separate rollback controller.

Upstream behavior and field reference are documented by Flux in
[OCI repositories](https://fluxcd.io/flux/components/source/ocirepositories/),
[Helm releases](https://fluxcd.io/flux/components/helm/helmreleases/), and the
[HelmRelease v2 API](https://fluxcd.io/flux/components/helm/api/v2/).
