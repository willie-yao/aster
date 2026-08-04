# Onboarding reference

This page documents discovery, automation, prompt drafting, validation, and the
full `fetcher onboard` command surface. For a first project, start with
[Onboarding a project](onboarding-a-new-project.md).

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

Repository metadata, Prow configuration, source excerpts, job metadata, and
model output are untrusted data. They cannot authorize commands, alter fixed
instructions, or request credentials. Documentation references may influence
deterministic ranking of eligible files in a pinned source snapshot, but cannot
trigger arbitrary URLs, commands, provider-time retrieval, or secret access.

## Accepted repository forms

The wizard accepts:

```text
owner/name
https://github.com/owner/name.git
ssh://git@github.com/owner/name.git
git@github.com:owner/name.git
```

If the current `origin` is a GitHub fork, the wizard can show the canonical
upstream and use it for Prow discovery after confirmation.

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

## AI provider and prompt drafting

The deployed analysis provider and the one-time prompt-drafting provider are
separate decisions.

Provider presets can seed the API mode and endpoint for GitHub Copilot, OpenAI,
NVIDIA, or a custom OpenAI-compatible provider. The model remains explicit
because model availability depends on the account and endpoint.

The wizard never asks for or stores a token. `AI_TOKEN` authenticates one-time
prompt drafting and is read only from the environment. It is never printed,
inspected, fingerprinted, or placed in the plan or generated files. Supply
deployment credentials through the documented GitHub Secret or Kubernetes
Secret path.

When prompt drafting is selected, the wizard:

1. Explains that documentation, source excerpts, and matched Prow job metadata
   may be sent.
2. Requires explicit confirmation.
3. Resolves the default branch to one commit and reads a bounded source corpus.
4. Reuses matched discovery and final-sweep jobs without another Prow discovery.
5. Makes one schema-bound evidence extraction call.
6. Validates the evidence, makes one schema-bound revision call, and validates
   the complete revision with the same rules.
7. Renders Markdown deterministically and validates the final section contract.
8. Falls back to a reviewable TODO template when extraction fails. If only
   revision fails, it renders the first validated evidence object.

The source corpus contains at most 10 Markdown, Go, YAML, or shell files or
line-ranged excerpts. One source contributes at most 20,000 bytes and all source
text contributes at most 80,000 bytes. Eligible files up to 1 MiB are scanned
before excerpt selection. Vendored, generated, binary, `node_modules`, and
`.github` paths are excluded. A truncated recursive Git tree is rejected.

Prow metadata includes job name, periodic or presubmit type, configuration file,
repository when established, branches or refs, and TestGrid annotations. It is
limited to 100 jobs and 40,000 bytes, with an omitted-count summary. Runtime
credentials are redacted from full source text before excerpting and from the
complete serialized provider input. The two structured completion stages and source retrieval
share a five-minute timeout. Each stage can use at most the existing schema,
forced-function, and bounded plain-JSON transport attempts, for six provider
requests total. Cancellation stops retrieval and provider calls. No third
free-form formatting stage is made.

Generated prompts are drafts. Review every architecture, artifact, failure, and
transient-classification claim before deployment.

The generated draft is an operational diagnostic runbook, not a repository
summary. It uses a fixed section order covering architecture, lifecycle, job
flavors, artifact layout, failure patterns, transient boundaries, triage,
source repositories, and unresolved details. Project-specific claims and exact
artifact paths must come from the bounded source corpus. Missing details remain
explicitly unresolved instead of being replaced with generic guidance.

Transient rules require positive evidence that permits the classification and a
boundary that makes the failure non-transient. The generator does not add common
transient classes when the source material is silent. The analyzer can inspect
supplied Prow artifacts. Optional Kubernetes tools navigate Kubernetes-shaped
logs and resource dumps already captured in the artifact tree. They do not
connect to a live Kubernetes API. The analyzer also does not have portal, SSH,
arbitrary shell, browser, or local CLI access.

When the source repository yields no meaningful source evidence, onboarding
skips the model request and writes the reviewable TODO template. Improve
repository artifact documentation or job metadata and rerun onboarding when the
generated `Unresolved details` section identifies important gaps. Choosing a
more capable model does not substitute for missing source evidence.

Prompt preparation records a credential-free result in the plan: requested mode,
final status, output type, safe failure stage and category, revision fallback,
and provider coordinates only for a successful API draft. The final review
shows `TODO template`, `Experimental API draft`, or `TODO template after
experimental API failure`.

Normal failures write a safe warning to stderr with the stage, category,
fallback, and a short action. They do not print the wrapped source or provider
error. Interactive API failures offer retry with the same reviewed coordinates,
continue with the TODO template, or cancel. Continue is the default.

Use `--no-prompt` to force the TODO template. Use `--ai=false` to disable
deployed AI analysis. These flags control different features.

`--prompt-debug` writes sanitized diagnostics to stderr only. It may include
stage timing, selected source paths and line ranges, source and job counts, API,
endpoint hostname, model fingerprint, structured transport attempt, HTTP status,
safe `Retry-After`, provider request ID, validation code and field, revision
fallback, and total elapsed time. It excludes credentials, source-line contents,
raw prompts, model responses, evidence text, provider bodies, endpoint query or
fragment details, credential-bearing URLs, and full private model identifiers.
No debug report file is created.

`--require-prompt-draft` is for strict automation. It is valid only for the
experimental API path, requires `AI_TOKEN`, `AI_ENDPOINT`, and `AI_MODEL`, and
returns a nonzero error before any local write or pull request when the final
result is not an API draft.

See [AI providers](ai-providers.md) and
[Writing the project prompt](writing-prompts.md).

## Dry-run behavior

`-dry-run` performs discovery, the real job sweep, planning, rendering,
destination checks, and strict configuration validation. It writes no files and
opens no pull request.

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -source-repo owner/source \
  -dry-run
```

An interactive dry run stops after review. A fully flagged dry run stays
non-interactive.

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

For automation that must receive an API-authored prompt rather than a safe
fallback, export the reviewed provider coordinates and add the strict flag:

```bash
export AI_TOKEN="..."
export AI_API="responses"
export AI_ENDPOINT="https://provider.example/v1/responses"
export AI_MODEL="reviewed-model"
fetcher onboard \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  --require-prompt-draft
```

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
