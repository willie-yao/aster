# Onboarding reference

This page documents discovery, automation, prompt authoring, validation, and the
full `fetcher onboard` command surface. For a first project, start with
[Onboarding a project](onboarding-a-new-project.md).

For a conversational agent workflow over the same command surface, see
[Agent-driven consumer setup](agent-onboarding.md).

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
to accept a choice or prefilled input, and `Ctrl+C` to cancel. When `TERM=dumb`,
the wizard uses equivalent numbered and line-oriented prompts. Set
`ACCESSIBLE=1` to select this mode in any terminal. Cancellation and EOF leave
no scaffold. The final confirmation defaults to no.

Repository metadata, Prow configuration, source files, job metadata, and model
output are untrusted data. They cannot authorize commands, alter fixed
instructions, or request credentials. Agent mode denies the shell tool and
accepts only one validated `prompts/system.md` change.

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
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest \
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

Orka remains a separate optional integration. The onboarding command never
installs, upgrades, or silently enables it.

## AI provider and prompt authoring

The deployed analysis provider and one-time prompt authoring are separate
decisions. Provider presets configure deployed analysis only. Prompt authoring
supports `agent`, `handoff`, and `todo-template`.

Agent mode resolves the source branch to an immutable commit, shallow-clones it
into a temporary checkout, and runs the local OpenCode process through pinned
`srt` OS sandboxing with a temporary config and its shell tool disabled. It uses
only the selected provider credential from the user's existing OpenCode
configuration and accepts one validated `prompts/system.md` change. Missing
`srt` safely falls back to the TODO template and handoff bundle.

Handoff mode writes the TODO template plus `PROMPT_HANDOFF.md` and the bundled
`.opencode/skills/system-prompt-generation/SKILL.md` without running an agent.
The handoff pins a commit when possible and otherwise records a known default
branch or an unresolved ref without inventing a branch name. Repository and Prow
metadata are serialized as untrusted data.

TODO-template mode writes only `prompts/system.md`. `--no-prompt` is an alias for
this mode and cannot be combined with another explicit prompt mode.

Complete flag-based runs default to handoff mode. The wizard recommends agent
mode and defaults its model to `github-copilot/claude-sonnet-4.6`. Override it
with `--prompt-agent-model=<provider/model>`. GitHub Copilot uses the reviewed
provider allowlist automatically. For another provider, repeat
`--prompt-network-domain=<domain[:port]>` for its reviewed destinations. A
malformed model or domain fails before source resolution or agent execution.

Prompt preparation records a credential-free result in the plan: requested mode,
final status, output type, timeout for agent mode, safe failure stage and category,
and the OpenCode runtime and model. Agent failures distinguish source revision,
agent execution, timeout, and deterministic output validation. Raw OpenCode
output is never included in the plan or safe warning.

`--require-prompt-draft` is for strict automation. It is valid only for `agent`
and returns a nonzero error before any local write or pull request when drafting
falls back.

`--prompt-timeout` controls source revision resolution and agent execution. It
defaults to `15m` and accepts values from `1m` through `2h`. This option does not
change the regular fetcher `--timeout` or the deployed project `ai.timeout`.

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

Every local plan classifies its generated files as `create` or `replace` before
the final confirmation. Without `--update-existing`, non-interactive onboarding
refuses any replacement. Interactive onboarding offers:

1. Choose another directory.
2. Update known scaffold files.
3. Cancel.

Choosing another directory is the default. Update mode replaces only files in
the validated plan. It preserves unrelated files, never deletes the destination,
and never removes stale files from another deployment or prompt mode. Existing
stale generated files are reported and left untouched. Partial
path conflicts, symbolic links in generated paths, and unsafe plan paths are
rejected.

`--update-existing` is local-only and cannot be combined with `--open-pr`.
Open-PR mode continues to submit the generated file map as a GitHub diff.

## Dry-run behavior

`-dry-run` performs discovery, the real job sweep, planning, rendering,
destination checks, and strict configuration validation. It prints the same
create/replace plan and stale-file warnings without writing scaffold files or
opening a pull request.

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
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
fetcher onboard \
  -apply-plan /private/path/onboard-plan.json \
  -plan-digest 'sha256:<reviewed-digest>'
```

The command rejects a changed digest, malformed artifact, unsupported schema,
symlinked plan file, invalid plan, or destination whose create/replace state or
reviewed replacement content no longer matches the review.

For an existing scaffold, the first non-interactive run without
`-update-existing` stops and lists conflicts. After the user authorizes those
replacement paths, rerun the dry run with `-update-existing` and `-plan-out`,
then review and apply that artifact.

## Non-interactive automation

When every required value is supplied, `onboard` does not prompt. Add
`-non-interactive` when automation must fail instead of prompting for a missing
value.

Pages example:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -out ./my-dashboard
```

Kubernetes example:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -mode k8s \
  -out ./my-dashboard
```

For a project outside Kubernetes TestGrid, replace `-testgrid` with:

```text
-bucket "<bucket>"
```

Add `-gcsweb-base "https://gcsweb.example.net/s3"` when the bucket is served
through gcsweb.

For automation that must receive a validated agent-authored prompt rather than a
handoff fallback, select agent mode and add the strict flag:

```bash
fetcher onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  --prompt-mode=agent \
  --prompt-agent-model=github-copilot/claude-sonnet-4.6 \
  --require-prompt-draft
```

The selected OpenCode provider must already be authenticated. The deployed
`AI_TOKEN` may be read only to prevent accidental serialization into nonsecret
fields; it is never sent during prompt authoring.

## Open a scaffold pull request

`-open-pr` is explicit. It opens a pull request against an existing dashboard
repository instead of writing a local directory.

```bash
export GITHUB_TOKEN="..."
fetcher onboard \
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
- Orka installation or runtime configuration.

If no Prow job or TestGrid annotation matches the source repository, the wizard
asks for a TestGrid dashboard or artifact bucket. It does not invent one.

## Validate an existing scaffold

Run the read-only doctor after generation or while diagnosing an existing
consumer:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest \
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
fetcher onboard
fetcher onboard discover
fetcher onboard doctor
```

Kubernetes bundle operations use:

```text
fetcher kubernetes install
fetcher kubernetes upgrade
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
- [Orka integration](orka.md)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)
