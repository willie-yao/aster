# Onboarding a project

`fetcher onboard` creates a validated consumer scaffold for GitHub Pages or
Kubernetes. It can guide an interactive first run from a GitHub source
repository, or preserve the existing fully flagged automation flow.

The command reuses the same discovery, category inference, templates, prompt
drafting, strict `project.yaml` loader, local writer, and pull request writer in
both modes. It does not maintain a second scaffold generator.

## What the wizard does

When required flags are missing and stdin is an interactive terminal, the
wizard:

1. Detects the current Git `origin`, or accepts `owner/name` or a GitHub URL.
2. Reads bounded GitHub repository metadata.
3. Reads Prow job definitions from one pinned `kubernetes/test-infra` revision.
4. Finds jobs whose presubmit repository or `extra_refs` test the source repo.
5. Ranks candidate TestGrid dashboards and lets you edit the selection.
6. Runs the real final job sweep and refuses a zero-job scaffold.
7. Suggests editable identity, dashboard repository, deployment, and categories.
8. Renders every file in memory and validates `project.yaml` with the real loader.
9. Shows the complete plan and intended paths.
10. Writes nothing until you answer yes to the final confirmation.

The interactive wizard uses keyboard forms. Use the arrow keys to move through
choices, Enter to accept a choice or prefilled input, and `Ctrl+C` to cancel.
Inferred values appear directly in their input fields so they can be edited
before continuing. When `TERM=dumb`, the wizard uses equivalent numbered and
line-oriented prompts without terminal cursor control. Set `ACCESSIBLE=1` to
select this screen-reader-friendly mode in any terminal. Cancellation and EOF
leave no scaffold. The final confirmation defaults to no.

Repository metadata, Prow configuration, and source documentation are treated
as untrusted data. Documentation is only passed as bounded input to the fixed
prompt-drafting contract. It cannot alter the CLI flow or cause command
execution.

## Guided first run

From the source repository checkout:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard
```

The wizard normally detects `origin`. If `origin` is a GitHub fork, it also
shows the canonical upstream repository and defaults Prow discovery to that
upstream after confirmation. Selection fields include a short description of
the highlighted choice. Text fields show inferred defaults as editable values,
not as hidden fallback behavior. You can instead supply a repository name or
URL:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -source-repo https://github.com/kubernetes-sigs/cluster-api-provider-azure
```

Accepted source forms include:

```text
owner/name
https://github.com/owner/name.git
ssh://git@github.com/owner/name.git
git@github.com:owner/name.git
```

For a private repository, export `GITHUB_TOKEN` before running the wizard. The
token is used for GitHub API access and is never printed, added to the plan, or
written into generated files.

## Read-only repository discovery

Inspect automatic inference without rendering or writing a scaffold:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard discover \
  -source-repo owner/name
```

Add `-json` for machine-readable output. The report includes:

- Normalized source repository and GitHub metadata source.
- Default branch and visibility.
- Matching Prow jobs.
- Ranked TestGrid candidates. Each candidate separates jobs that directly test
  the source repository from the dashboard's complete periodic, presubmit, and
  postsubmit tab counts. Repository-match counts drive ranking; dashboard totals
  describe what the selected TestGrid contains.
- Suggested project identity and dashboard repository.
- Warnings and unresolved fields.
- The pinned `kubernetes/test-infra` revision.

Discovery is read-only. It does not render files, create repositories, write
GitHub settings, or inspect a Kubernetes cluster.

The dashboard fetcher currently ingests periodic jobs and optional presubmits.
Postsubmit tabs are reported during discovery so TestGrid totals are transparent,
but postsubmit artifact ingestion is not supported.

## Deployment profiles

`project.yaml` owns portable project behavior and analysis policy. Workflow
inputs and Helm values own infrastructure, credentials, and execution tuning.

### GitHub Pages

Choose Pages when artifacts are publicly readable, the model provider is
reachable from GitHub Actions, and authenticated server actions are not
required.

The scaffold contains:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml
CHECKLIST.md
```

When AI is enabled, complete the generated checklist by setting:

```text
AI_API
AI_ENDPOINT
AI_MODEL
AI_TOKEN
```

`AI_API`, `AI_ENDPOINT`, and `AI_MODEL` are repository variables. `AI_TOKEN` is
a repository secret. The wizard does not write any of them to GitHub.

### Kubernetes with Helm

Choose Kubernetes when the endpoint is cluster-local, persistent server state
is needed, authenticated actions are required, or continuous watch mode is
desired.

The scaffold contains:

```text
project.yaml
prompts/system.md
skills/*.yaml
deploy/values.yaml
deploy/README.md
```

The `skills/` directory is optional unless `project.yaml` requires consumer
recipes. When AI is enabled, generated values reference the predictable
`<release>-ai` Kubernetes Secret and key `AI_TOKEN`. Create that Secret with
your normal secret manager before installation. The wizard never writes its
value.

The wizard generates files only. It does not install Helm releases, write
Kubernetes Secrets, configure ingress or DNS, or inspect a cluster. From an
engine checkout, build the helper with `make build`, then validate the bundle
without cluster writes. Replace the chart-version placeholder with a published
release. Live installs and upgrades require Helm 4.

```bash
./bin/fetcher kubernetes install \
  --project-dir ../my-dashboard \
  --values deploy/values.yaml \
  --release my-dashboard \
  --namespace dashboards \
  --kube-context my-cluster \
  --chart-version <chart-version> \
  --dry-run
```

Remove `--dry-run` for the fresh install. Later image, values, project, prompt,
or skill changes use the same flags with `kubernetes upgrade`. Live commands
wait and roll back on failure; upgrades also reuse deployed values. Image-only
upgrades do not require editing
`project.yaml`. The wrapper validates the current bundle and passes its files to
the chart-managed ConfigMap on every run. See
[Kubernetes with Helm](kubernetes.md#install-and-upgrade-a-consumer-bundle) for
the published OCI chart and manual Helm equivalent.

Orka remains a separate advanced integration. The first-run scaffold does not
install, upgrade, or silently enable Orka.

## AI configuration and prompt drafting

The wizard separates two decisions:

1. The provider used by the deployed dashboard.
2. Whether to use a provider now to draft `prompts/system.md` from bounded
   source-repository documentation.

For the deployed dashboard, choose one of these paths:

- GitHub Copilot.
- OpenAI Responses.
- OpenAI Chat Completions.
- NVIDIA API.
- A self-hosted OpenAI-compatible endpoint such as Ollama, vLLM, NIM, or Ray Serve.
- Azure OpenAI or an Azure gateway.
- Another custom Chat Completions or Responses endpoint.
- Configure later.

Public-provider presets fill the API mode and endpoint as editable values. The
wizard always requires you to enter a model that your account or deployment
actually exposes. It never guesses a model or requests a token.

`Configure later` produces a valid initial scaffold with AI disabled. The
generated `CHECKLIST.md` or `deploy/README.md` explains how to enable it later.
It does not leave an enabled deployment with missing provider coordinates.

In Pages mode, obvious HTTP, localhost, loopback, private-IP, and cluster-local
endpoints produce a warning that defaults to no. Rejecting the warning returns
to provider selection. This check does not make a network request and cannot
prove that other endpoints are reachable. Kubernetes mode accepts cluster-local
HTTP endpoints without that Pages warning.

Azure deployments commonly require an `api-key` header. The stock Pages
workflow sends `AI_TOKEN` as a bearer token, so use a trusted gateway or
separately managed private configuration when direct Azure authentication needs
that header.

The same endpoint and model may be used for both deployed analysis and prompt
drafting, but the wizard asks before sending documentation to the provider.
Always review an AI-generated prompt before deployment.

Supported environment variables remain:

```text
AI_API
AI_ENDPOINT
AI_MODEL
AI_TOKEN
GITHUB_TOKEN
```

Tokens remain environment-only. Do not put tokens in an endpoint URL. The
onboarding plan and generated files contain no token, password, cookie,
kubeconfig, or Kubernetes Secret value.

Use `-no-prompt` to force the reviewable prompt stub. This flag only controls AI
prompt drafting. It does not disable terminal interaction. Use `-ai=false` to
disable deployed AI analysis.

## Dry run

`-dry-run` performs discovery, the real job sweep, planning, rendering, output
path checks, and strict configuration validation. It writes no files and opens
no pull request.

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -source-repo owner/source \
  -dry-run
```

The interactive dry run stops after the review. A fully flagged dry run remains
non-interactive.

## Non-interactive automation

When all required flags are supplied, `onboard` never prompts. Add
`-non-interactive` when automation must fail rather than prompt if a required
value is missing.

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

For projects outside Kubernetes TestGrid, replace `-testgrid` with an artifact
bucket:

```bash
-bucket "<bucket>"
```

Add `-gcsweb-base "https://gcsweb.example.net/s3"` when the bucket is served
through gcsweb.

## Open a scaffold pull request

`-open-pr` is always explicit. It opens a pull request against an existing
dashboard repository instead of writing a local directory.

```bash
export GITHUB_TOKEN="..."
fetcher onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<existing-dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -open-pr
```

The command does not create the repository. It does not enable Pages or write
variables and Secrets. `-open-pr -dry-run` plans the pull request without
opening it.

## Review the result

Before deployment:

1. Confirm discovery, storage, branding, and source repository in
   `project.yaml`.
2. Review, reorder, rename, or remove inferred categories.
3. Review every claim in `prompts/system.md` and replace any TODOs.
4. Follow `CHECKLIST.md` for Pages or `deploy/README.md` for Kubernetes.

Do not add notifications, issue automation, fix-PR automation, source
investigation, or Orka settings until the first fetch publishes the expected
jobs.

## Automatic inference limits

The wizard does not guess operational settings that GitHub and Prow metadata
cannot establish safely. It does not infer:

- AI provider reachability.
- Kubernetes namespace or storage class.
- Ingress, DNS, certificates, or OAuth.
- Notification routing.
- Secret values.
- Orka installation or runtime configuration.

If no Prow jobs or TestGrid annotations match the source repository, the wizard
asks for a TestGrid dashboard or artifact bucket. It never invents one.

## Validate an existing scaffold

Run the read-only doctor after generation or when diagnosing an existing
consumer:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard doctor \
  -project-dir ./my-dashboard
```

Doctor checks:

- Strict `project.yaml` parsing.
- Presence of a non-empty `prompts/system.md`.
- The Pages reusable-workflow target, effective `project_dir`, AI inputs, and
  token mapping.
- Kubernetes persistence strategy, AI provider coordinates, and credential
  source.
- The real Prow discovery sweep and a nonzero job count.

Failures include the next corrective action and return a nonzero exit status.
Warnings identify settings that cannot be resolved offline, such as dynamic
GitHub expressions, repository variable values, or a Helm token supplied only
at install time. Doctor does not contact the model provider or inspect a
Kubernetes cluster. Those checks remain deferred until they have separate,
explicit opt-in contracts.

## Command surface

Scaffolding remains under `fetcher onboard`. Kubernetes bundle installation and
upgrades use `fetcher kubernetes install|upgrade`, which reuses the same binary
and project loaders. A separate top-level `prow-ai-dashboard` executable was
not added because it would duplicate command distribution and validation.

## Deploy and validate

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes with Helm](kubernetes.md)
- [Separate Orka Helm installation](kubernetes.md#install-orka-as-a-separate-release)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)

A successful first deployment has the expected branding in
`data/manifest.json`, at least one job in `data/dashboard.json`, grounded
analysis when AI is enabled, and healthy server endpoints in Kubernetes mode.
