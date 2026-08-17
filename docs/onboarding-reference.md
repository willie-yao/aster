# Onboarding reference

This page documents discovery, automation, prompt authoring, validation, and the
full `aster onboard` command surface. It includes advanced and experimental
flags that do not belong in the first-run path. For a first project, start with
[Onboarding a project](onboarding-a-new-project.md).

For a conversational agent workflow over the same command surface, see
[Agent-driven setup and diagnostic authoring](agent-onboarding.md).

Commands that use `go run` below pin the current prerelease exactly. An
installed `aster` CLI from the same release can be used instead. Use an exact
stable tag once one is published; reserve commit pins for engine development.
For Pages scaffolds, `-engine-ref` must also pin the generated reusable workflow.
The flag does not select Kubernetes image tags or chart versions.

## Discovery behavior

When required flags are missing and stdin is an interactive terminal, the
wizard:

1. Detects the current Git `origin`, or accepts a GitHub repository directly.
2. Reads bounded GitHub repository metadata.
3. Reads Prow job definitions from one pinned `kubernetes/test-infra` revision.
4. Finds jobs whose presubmit repository or `extra_refs` test the source repo.
5. Ranks candidate TestGrid dashboards and lets the user edit the selection.
6. Runs the real final job sweep and refuses a zero-job scaffold.
7. Suggests editable identity, dashboard repository, deployment, and categories.
8. Renders every file in memory and validates `project.yaml` with the real loader.
9. Shows the complete plan and destination paths.
10. Writes nothing until the final confirmation.

The wizard uses the same discovery, category inference, templates, prompt
builder, strict loader, local writer, and pull request writer as the fully
flagged path. It does not maintain a separate scaffold generator.

The interactive wizard uses keyboard forms. Use the arrow keys to move, Enter
to accept a choice or prefilled input, Esc to clear the current text field, and
`Ctrl+C` to cancel. When `TERM=dumb`,
the wizard uses equivalent numbered and line-oriented prompts. Set
`ACCESSIBLE=1` to select this mode in any terminal. Cancellation and EOF leave
no scaffold. The final confirmation defaults to no.

Repository metadata, Prow configuration, source files, job metadata, and model
output are untrusted data. They cannot authorize commands, alter fixed
instructions, or request credentials. Handoff mode serializes them only as
reviewable context for the operator's own coding agent.

## Accepted repository forms

The wizard accepts:

```text
owner/name
https://github.com/owner/name.git
ssh://git@github.com/owner/name.git
git@github.com:owner/name.git
```

If the current `origin` is a GitHub fork, the wizard can show the canonical
upstream and use it for Prow discovery after confirmation. The source repository
and dashboard destination remain separate: selecting the upstream for Prow
discovery does not suggest creating the dashboard under the upstream owner.

The dashboard repository suggestion prefers the authenticated GitHub login when
`GITHUB_TOKEN` can safely identify it. Otherwise it uses the owner of the Git
remote that onboarding detected. If neither is available, the wizard leaves the
owner empty and requires an explicit `owner/name`. An explicitly supplied
`--dashboard-repo` is always preserved.

The optional short name starts empty. Repository initials are not reliable
project abbreviations, so enter an established abbreviation explicitly when the
project has one.

For private repositories, export `GITHUB_TOKEN`. The token is used only for
GitHub API access. It is not printed, retained in the plan, or written to the
scaffold.

## Read-only discovery

Inspect automatic inference without rendering or writing files:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 \
  onboard discover \
  -source-repo owner/name
```

Add `-json` for machine-readable output. The report includes:

- Normalized source repository and metadata source.
- Default branch and visibility.
- Matching Prow jobs.
- Ranked TestGrid candidates.
- Suggested project identity and dashboard repository.
- Warnings and unresolved fields.
- The pinned `kubernetes/test-infra` revision.

This revision makes the discovery operation internally consistent but is not
written as a permanent consumer pin by default. For a reproducible evaluation,
copy it into `discovery.test_infra_revision` after scaffolding and before the
first fetch or deployment, then run `onboard doctor` again. Normal dashboards
can leave the field unset to follow current job configuration.

Each TestGrid candidate separates direct source matches from the complete
periodic, presubmit, and postsubmit tab counts. Direct source matches drive
ranking. Dashboard totals describe the selected TestGrid dashboard.

The fetcher ingests periodic jobs and optional presubmits. Postsubmit tabs are
reported for transparent discovery totals, but postsubmit artifact ingestion is
not supported.

Discovery does not render files, create repositories, change GitHub settings,
or inspect a Kubernetes cluster.

## Deployment profiles

`project.yaml` owns portable behavior and analysis policy. GitHub workflow
inputs and Helm values own infrastructure, credentials, and execution tuning.

### GitHub Pages

Use Pages when artifacts are publicly readable, the model endpoint is reachable
from GitHub Actions, and authenticated server features are not required.

The generated workflow reads these repository settings when AI is enabled:

```text
AI_API       repository variable
AI_ENDPOINT  repository variable
AI_MODEL     repository variable
AI_REASONING_EFFORT  optional repository variable
AI_TOKEN     repository Secret
```

The wizard never writes these values to GitHub. Follow the generated
`CHECKLIST.md` and [GitHub Actions and Pages](github-pages.md).

Cluster-local, loopback, private-address, and insecure HTTP endpoints are not
reachable safely from GitHub-hosted runners. The wizard warns before accepting
such a Pages endpoint.

### Kubernetes

Use Kubernetes when the provider endpoint is private to the cluster, output and
cache data need persistent shared storage, or server features are required.

The generated bundle contains:

```text
project.yaml
prompts/system.md
deploy/values.yaml
deploy/README.md
```

The wizard seeds deployment values but does not inspect a cluster, choose a
storage class, create a namespace or Secret, install Helm releases, or configure
DNS and ingress. Follow the generated guide or the
[Kubernetes quickstart](kubernetes.md).

Standard onboarding configures the in-process analyzer. No external analysis
runtime is installed, upgraded, or enabled by this command.

## AI provider and prompt authoring

The deployed analysis provider and one-time prompt authoring are separate
decisions. Provider presets configure deployed analysis only. Prompt authoring
supports `handoff` and `todo-template`.

Handoff mode writes the TODO template plus `PROMPT_HANDOFF.md` and the bundled
`.opencode/skills/system-prompt-generation/SKILL.md` without running an agent.
The handoff pins a commit when possible and otherwise records a known default
branch or an unresolved ref without inventing a branch name. Repository and Prow
metadata are serialized as untrusted data.

TODO-template mode writes only `prompts/system.md`. `--no-prompt` is an alias for
this mode and cannot be combined with another explicit prompt mode.

Complete flag-based runs and the wizard default to handoff mode. The generated
bundle is meant for the operator to run with their own coding agent, then review
and copy the resulting `prompts/system.md` into the consumer repository.

Prompt preparation records a credential-free result in the plan: requested mode,
final status, output type, and source-resolution status. No model output is
included in the plan or safe warning.

`--prompt-timeout` bounds prompt source resolution. It defaults to `15m` and
accepts values from `1m` through `2h`. This option does not change the regular
fetcher `--timeout` or the deployed project `ai.timeout`.

Generated prompts are drafts. Review every architecture, artifact, failure, and
transient-classification claim before deployment.

See [Writing the project prompt](writing-prompts.md).

## Scaffold destination and local updates

The scaffold belongs in the dashboard consumer repository, not inside the
source repository. When onboarding detects the source from the current Git
checkout, the interactive default is the sibling
`../<dashboard-repository-name>`. For an explicitly supplied source without a
detected checkout, the default remains a safe relative directory in the current
working directory. `--out` always wins and may point to an existing checkout.

Every local plan records `create`, `replace`, or `preserve` plus
`engine_generated` or `consumer_owned` ownership before final confirmation.
Without `--update-existing`, non-interactive onboarding refuses replacement of
engine-generated files. Interactive onboarding offers:

1. Choose another directory.
2. Update known scaffold files.
3. Cancel.

Choosing another directory is the default. Update mode replaces only
engine-generated files in the validated plan. Existing `prompts/system.md` and
`skills/*.yaml` or `skills/*.yml` are consumer-owned and preserved by default.
The plan records the existing prompt hash and the generated source-only candidate
hash. Existing skills are always preserved.

`--replace-consumer-owned` requires `--update-existing` and permits only an
explicitly reviewed `prompts/system.md` replacement. It does not replace skills.
Update mode preserves unrelated files, never deletes the destination, and leaves
stale generated files untouched. Partial path conflicts, symbolic links, and
unsafe paths are rejected. Local update flags cannot be combined with
`--open-pr`.

## Dry-run behavior

`-dry-run` performs discovery, the real job sweep, source revision pinning,
planning, rendering, destination checks, and strict configuration validation.
It prints engine/source/catalog identities, the discovery digest, deployment
rationale, prompt hashes, and the same create/replace/preserve plan without
writing scaffold files or opening a pull request.

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 onboard \
  -engine-ref v0.9.0-rc.2 \
  -source-repo owner/source \
  -dry-run
```

An interactive dry run stops after review. A fully flagged dry run stays
non-interactive.

`-plan-out <path>` is valid only with `-dry-run`. It writes a versioned,
credential-free artifact containing the exact rendered files and reviewed
destination actions. The output prints a `sha256:` digest for that file. The
path must not already exist. Local destinations are stored as canonical absolute
paths with existing symlink ancestors resolved. Apply rechecks that target so a
different working directory or retargeted ancestor cannot redirect the reviewed
scaffold. The plan artifact must be outside the consumer destination and cannot
represent an open-PR plan.

Apply the exact reviewed artifact with no discovery or scaffold flags:

```bash
aster onboard \
  -apply-plan /private/path/onboard-plan.json \
  -plan-digest 'sha256:<reviewed-digest>' \
  -result-out /private/path/manifest/apply-result.json \
  -handoff-out /private/path/manifest/setup-handoff.json \
  -artifact-smoke-builds 1
```

The command rejects a changed digest, malformed artifact, unsupported schema,
symlinked plan file, invalid plan, or destination whose reviewed create, replace,
preserve, ownership, or content state changed. After writing, it emits a
deterministic file manifest, runs doctor, and performs a read-only artifact
usability check for recent builds, `prowjob.json`, `started.json`,
`build-log.txt`, JUnit, and `artifacts/`. When every sampled build lacks JUnit,
the handoff warns that test-level granularity may be unavailable. The setup
handoff also records first-class artifact-location and test-infra identities for
diagnostic authoring without diagnosing failures.

For an existing scaffold, the first non-interactive run without
`-update-existing` stops and lists engine-generated conflicts. After approval,
rerun with `-update-existing` and `-plan-out`. Confirm that the prompt and skills
are preserved. A prompt replacement needs a reviewed diff, separate approval,
and a new plan with `-replace-consumer-owned`.

## Non-interactive automation

When every required value is supplied, `onboard` does not prompt. Add
`-non-interactive` when automation must fail instead of prompting for a missing
value.

Pages example:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 onboard \
  -engine-ref v0.9.0-rc.2 \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -artifact-access public \
  -deployment-reason "Artifacts and provider are reachable from GitHub Actions." \
  -out ./my-dashboard
```

Kubernetes example:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -mode k8s \
  -artifact-access private \
  -deployment-reason "Artifacts require in-cluster authenticated access." \
  -out ./my-dashboard
```

The Kubernetes example omits `-engine-ref` because Kubernetes image and chart
versions are selected by the deployment values and release commands, not by the
Pages workflow ref.

For a project outside Kubernetes TestGrid, replace `-testgrid` with:

```text
-bucket "<bucket>"
```

For a bounded evaluation, repeat `-exact-job` with bucket discovery:

```text
-bucket "kubernetes-ci-logs"
-exact-job "periodic-project-e2e"
-exact-job "periodic-project-upgrade"
```

Exact-job discovery validates the named direct bucket indexes and fails when a
name is missing. It cannot be combined with `-testgrid`.

Add `-gcsweb-base "https://gcsweb.example.net/s3"` when the bucket is served
through gcsweb.

For automation that should not emit a handoff bundle, select the template-only
mode or use `--no-prompt`:

```bash
aster onboard \
  -engine-ref v0.9.0-rc.2 \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  --prompt-mode=todo-template
```

The deployed `AI_TOKEN` may be read only to prevent accidental serialization into
nonsecret fields; it is never sent during prompt authoring.

## Open a scaffold pull request

`-open-pr` is explicit. It opens a pull request against an existing dashboard
repository instead of writing a local directory.

```bash
export GITHUB_TOKEN="..."
aster onboard \
  -engine-ref v0.9.0-rc.2 \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<existing-dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -open-pr
```

The command does not create the repository, enable Pages, or write variables and
Secrets. `-open-pr -dry-run` plans the pull request without creating it.

## Automatic inference limits

The wizard does not infer settings that repository and Prow metadata cannot
establish safely. It does not guess:

- AI provider reachability.
- Kubernetes context, namespace, or storage class.
- Ingress, DNS, certificates, or OAuth.
- Notification routing.
- Secret values.
- Optional feature runtime installation or configuration.

If no Prow job or TestGrid annotation matches the source repository, the wizard
asks for a TestGrid dashboard or artifact bucket. It does not invent one.

## Validate an existing scaffold

Run the read-only doctor after generation or while diagnosing an existing
consumer:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 \
  onboard doctor \
  -project-dir ./my-dashboard
```

Doctor checks:

- Strict `project.yaml` parsing.
- A non-empty `prompts/system.md`.
- Pages workflow target, effective project directory, AI inputs, and token map.
- Kubernetes persistence, provider coordinates, and credential source.
- The real Prow discovery sweep and a nonzero job count.

Failures include the next corrective action and return a nonzero exit status.
Warnings identify values that cannot be resolved offline, such as GitHub
expressions, repository variables, or a provider token supplied at deployment.
Doctor does not contact the model provider or inspect a Kubernetes cluster.

## Command surface

Scaffolding and read-only validation remain under:

```text
aster onboard
aster onboard discover
aster onboard doctor
```

Kubernetes bundle operations use:

```text
aster kubernetes install
aster kubernetes upgrade
```

These commands reuse the same project, prompt, and skill validation. A separate
top-level executable is not required.

## Review and deployment

Before deployment:

1. Confirm discovery, storage, branding, and source repository.
2. Review inferred categories.
3. Review every claim in `prompts/system.md`.
4. Follow `CHECKLIST.md` or `deploy/README.md`.
5. Deploy the smallest working configuration before optional automation.

A successful first deployment has the expected branding in
`data/manifest.json`, at least one job in `data/dashboard.json`, grounded
analysis when AI is enabled, and healthy server endpoints in Kubernetes mode.

Related guides:

- [Onboarding quickstart](onboarding-a-new-project.md)
- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes quickstart](kubernetes.md)
- [Kubernetes operator reference](kubernetes-reference.md)
- [Project configuration](project-configuration.md)
- [Optional features](optional-features.md)
- [Troubleshooting](troubleshooting.md)
- [Complete documentation map](README.md)
