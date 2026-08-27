# Onboarding reference

This page documents advanced discovery, plan and apply automation, update
behavior, and the `aster onboard` command surface. Start a first project with
[Onboarding a project](onboarding-a-new-project.md).

Examples use the current release exactly. An installed `aster` binary from the
same release can replace `go run`. Pages scaffolds also require an exact
`-engine-ref`; Kubernetes image and chart versions are selected by deployment
values and release commands.

## Discovery

### Accepted repository forms

Source and dashboard repositories accept:

```text
owner/name
https://github.com/owner/name.git
ssh://git@github.com/owner/name.git
git@github.com:owner/name.git
```

When the current `origin` is a fork, onboarding can use the confirmed canonical
upstream for Prow discovery while keeping the dashboard destination separate.
An explicit `-dashboard-repo` is never replaced by inference.

For private repositories, export `GITHUB_TOKEN`. It is used for bounded GitHub
API reads and is not printed, retained in the plan, or written to the scaffold.

### Read-only discovery

Inspect inferred inputs without rendering files:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.9 \
  onboard discover \
  -source-repo owner/name
```

Add `-json` for machine-readable output. The report includes the normalized
source repository, matching Prow jobs, ranked TestGrid candidates, suggested
identity and dashboard destination, unresolved fields, and the pinned
`kubernetes/test-infra` revision used for that discovery operation.

The revision is not written as a permanent consumer pin by default. Set
`discovery.test_infra_revision` after scaffolding only when a reproducible
consumer must keep that exact catalog revision.

Discovery reads bounded repository metadata and one pinned Prow configuration,
finds jobs whose presubmit repository or `extra_refs` match the source, ranks
TestGrid candidates, and refuses a final zero-job scaffold. It reports periodic,
presubmit, and postsubmit tab counts, but the fetcher ingests only periodic jobs
and optional presubmits.

Discovery does not render files, create repositories, change GitHub settings,
call a model provider, or inspect a Kubernetes cluster.

### Interactive behavior

When required flags are missing and stdin is a terminal, the wizard uses the
same discovery, renderer, strict project loader, and writers as the flagged
path. It shows the complete final plan and defaults the final confirmation to
no. `Ctrl+C`, EOF, or cancellation leaves no scaffold.

Set `ACCESSIBLE=1` or use `TERM=dumb` for numbered and line-oriented prompts.
Repository metadata, Prow configuration, source files, and generated handoff
context are untrusted data. They cannot authorize commands or request
credentials.

## Deployment profiles

`project.yaml` owns portable discovery, branding, and analysis policy.
Workflows and Helm values own infrastructure, credentials, image selection,
persistence, and runtime tuning.

### GitHub Pages

Pages requires publicly readable artifacts and a provider reachable from the
runner. When AI is enabled, the generated workflow reads:

```text
AI_API                 repository variable
AI_ENDPOINT            repository variable
AI_MODEL               repository variable
AI_REASONING_EFFORT    optional repository variable
AI_TOKEN               repository Secret
```

Onboarding never writes these settings to GitHub. Cluster-local, loopback,
private-address, and insecure HTTP endpoints are not safe Pages targets. See
[GitHub Actions and Pages](github-pages.md).

### Kubernetes

Kubernetes supports private artifact or provider access, persistent shared
state, and authenticated server features. The generated bundle contains:

```text
project.yaml
prompts/system.md
deploy/values.yaml
deploy/README.md
```

Onboarding does not inspect a cluster or choose storage. Interactive runs leave
a reviewed placeholder for later editing. Non-interactive Kubernetes runs
require exactly one of `-k8s-storage-class` or `-k8s-existing-claim` so reviewed
plan application can pass static doctor. The command does not create a namespace
or Secret, install Helm releases, or configure DNS and ingress. It configures
authoritative in-process analysis only. Follow the generated guide and
[Kubernetes quickstart](kubernetes.md).

## Prompt handoff

Prompt authoring is separate from the deployed provider. `handoff` mode writes a
TODO prompt, `PROMPT_HANDOFF.md`, and the bundled
`.opencode/skills/system-prompt-generation/SKILL.md` without running a model.
`todo-template` writes only `prompts/system.md`; `--no-prompt` is an alias.

Complete flagged runs and the wizard default to `handoff`. The handoff records a
resolved commit when possible, otherwise a known branch or unresolved ref. It
serializes repository and Prow metadata as untrusted review context. It never
uses the deployed `AI_TOKEN`, `AI_ENDPOINT`, `AI_MODEL`, or
`AI_REASONING_EFFORT`.

`--prompt-timeout` bounds source resolution from `1m` through `2h` and defaults
to `15m`. Review every generated architecture, artifact, transient, and failure
claim. See [Writing the project prompt](writing-prompts.md).

## Destination and updates

The consumer normally lives in its own repository, not in the monitored source
repository. Local output must be empty or absent unless update mode is explicitly
selected.

For an existing consumer, the first run without `-update-existing` lists
conflicts and stops. After review, rerun with `-update-existing`. Engine-owned
files may be replaced, but existing `prompts/system.md` and `skills/*.yaml` or
`skills/*.yml` remain consumer-owned and are preserved. Replacing the prompt
requires a separate diff, explicit approval, and a new plan with
`-replace-consumer-owned`. Existing active skills are never replaced by setup.

The command never deletes stale or unrelated files.

## Dry-run and reviewed plan application

`-dry-run` renders and validates the complete scaffold without writing it.
`-plan-out` writes a private machine-readable plan outside the destination. A
Kubernetes saved plan requires `-k8s-storage-class` or
`-k8s-existing-claim` so exact application can pass static doctor:

```bash
aster onboard \
  <complete-inputs> \
  -dry-run \
  -plan-out /private/path/onboard-plan.json
```

The plan records exact engine, source, catalog, job, destination, prompt, and
file-action identities plus a digest. After reviewing the plan, apply only that
artifact:

```bash
aster onboard \
  -apply-plan /private/path/onboard-plan.json \
  -plan-digest 'sha256:<reviewed-digest>' \
  -result-out /private/path/manifest/apply-result.json \
  -handoff-out /private/path/manifest/setup-handoff.json \
  -artifact-smoke-builds 1
```

Application fails closed if the digest, schema, source identities, destination
state, file ownership, or reviewed create, replace, and preserve actions changed.
It rejects symlinked plan files. After writing, it emits a deterministic file
manifest, runs doctor, and performs a bounded read-only artifact smoke check.

The handoff records artifact location, test-infra identity, and whether sampled
builds contain JUnit. It does not diagnose failures. Keep plan, result, and
handoff artifacts outside the consumer destination.

## Non-interactive automation

Add `-non-interactive` when missing values must fail rather than prompt.

The common Pages command is in
[Non-interactive CLI onboarding](onboarding-a-new-project.md#non-interactive-cli-onboarding).
Use the options below only when the automation contract differs from that common
path.

Kubernetes automation requires a CLI whose `onboard -h` output includes
`-k8s-storage-class` and `-k8s-existing-claim`. Use a current clean engine
checkout or a later exact release containing that command surface.

Kubernetes example:

```bash
aster onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -mode k8s \
  -k8s-storage-class "<rwx-storage-class>" \
  -artifact-access private \
  -deployment-reason "Artifacts require in-cluster authenticated access." \
  -out ./my-dashboard
```

For direct bucket discovery, replace `-testgrid` with `-bucket`. A bounded exact
job evaluation may repeat `-exact-job` with bucket discovery:

```text
-bucket "kubernetes-ci-logs"
-exact-job "periodic-project-e2e"
-exact-job "periodic-project-upgrade"
```

Exact-job discovery fails when a named bucket index is absent and cannot be
combined with `-testgrid`. Add `-gcsweb-base` when the bucket is served through
gcsweb.

Use `--prompt-mode=todo-template` or `--no-prompt` when automation should not
emit the prompt handoff bundle.

## Open a scaffold pull request

`-open-pr` is explicit and targets an existing dashboard repository:

```bash
export GITHUB_TOKEN="..."
aster onboard \
  -engine-ref v0.9.0-rc.9 \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<existing-dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -open-pr
```

The command does not create the repository, enable Pages, write variables or
Secrets, or deploy the result. `-open-pr -dry-run` plans the pull request without
creating it. Agent-assisted setup normally uses local plan application first so
doctor, artifact smoke, and handoff validation can run before a separately
confirmed pull request.

## Inference limits

Onboarding does not guess:

- provider reachability or credential validity;
- Kubernetes context, namespace, ingress, DNS, or certificates;
- which ReadWriteMany StorageClass or existing PVC the operator should select;
- OAuth, notification routing, or Secret values;
- optional Agent Sandbox feature installation;
- a TestGrid dashboard or artifact bucket when no source match exists.

## Validate an existing consumer

Run the read-only `onboard doctor` command from the
[onboarding quickstart](onboarding-a-new-project.md#validate-with-onboard-doctor).
Doctor checks strict project parsing, a non-empty prompt, Pages or Kubernetes
coordinates, and a real nonzero Prow discovery sweep. Warnings identify values
that cannot be resolved offline. It does not call the provider or inspect a
Kubernetes cluster.

## Command surface

```text
aster onboard
aster onboard discover
aster onboard doctor
aster kubernetes install
aster kubernetes upgrade
```

The Kubernetes commands reuse the same project, prompt, and skill validation.
Deployment details belong in [Kubernetes quickstart](kubernetes.md) and the
[Kubernetes operator reference](kubernetes-reference.md).
