# Onboarding a project

`fetcher onboard` creates a validated consumer scaffold for GitHub Pages or
Kubernetes. Start with the guided wizard, review the generated files, then
follow the deployment guide included in the scaffold.

## Run the wizard

From the source repository checkout:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard
```

The wizard normally detects the GitHub repository from `origin`. You can also
provide it explicitly:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -source-repo kubernetes-sigs/cluster-api-provider-azure
```

For a private repository, export `GITHUB_TOKEN` before starting. The token is
used for GitHub API reads. It is never printed or written into generated files.

The wizard asks you to choose or confirm:

1. The source repository.
2. A TestGrid dashboard or artifact bucket.
3. GitHub Pages or Kubernetes deployment.
4. Project and dashboard names.
5. Whether to include presubmit jobs.
6. Whether to enable AI analysis and which provider to use.
7. Whether to use the experimental API path to draft `prompts/system.md` from bounded source evidence and matched Prow job metadata.
8. The output directory or pull request destination.

In the interactive form, use the arrow keys to move, Enter to select, and
`Ctrl+C` to cancel. Inferred text values are prefilled and editable. The final
confirmation defaults to no.

The dashboard repository must be a consumer repository you control. The wizard
prefers the authenticated GitHub login, then the owner of an automatically
detected Git remote. Selecting an upstream fork source for Prow discovery does
not change that destination owner. If no safe owner is known, enter `owner/name`
explicitly. The optional short name starts empty because repository initials do
not reliably identify established project abbreviations.

## Choose a deployment

### GitHub Pages

Choose Pages for a public read-only dashboard when the artifact store and model
endpoint are reachable from GitHub Actions.

The scaffold contains:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml
CHECKLIST.md
```

After generation, follow `CHECKLIST.md` to configure GitHub Pages, repository
variables, and the `AI_TOKEN` repository Secret.

### Kubernetes

Choose Kubernetes for a private in-cluster model endpoint, persistent shared
data, or authenticated server features.

The scaffold contains:

```text
project.yaml
prompts/system.md
deploy/values.yaml
deploy/README.md
```

After generation, make `deploy/values.yaml` your deployment configuration and
follow the copyable commands in `deploy/README.md`. The generated values keep
common settings active and optional features commented. The header links to the
complete values and the matching `values.schema.json`; Helm validates supplied
values against the schema before rendering. The engine quickstart is in
[Kubernetes deployment](kubernetes.md).

Orka is optional. The normal Kubernetes deployment uses the in-process analysis
runtime and does not install or require Orka.

## Review before writing

The wizard renders and validates the complete plan in memory before it writes
anything. Review:

- The selected jobs and discovery source.
- Project identity and dashboard repository.
- Inferred categories in `project.yaml`.
- Every project-specific claim in `prompts/system.md`.
- The deployment files and their destination paths.

Repository metadata, Prow configuration, source excerpts, and job metadata are
untrusted input. They cannot alter the wizard flow or cause command execution.
Repository content is sent to a prompt-drafting provider only after explicit
confirmation. `AI_TOKEN` authenticates that one-time draft and remains an
environment-only value. It is never displayed, inspected, fingerprinted, or
written into the plan or scaffold.

Prompt drafting pins the source repository to one commit and sends at most 10
line-ranged Markdown, Go, YAML, or shell excerpts, with a 20,000-byte per-source
limit and an 80,000-byte total. Prow metadata is separately limited to 100 jobs
and 40,000 bytes. Documentation references may raise the rank of exact eligible
files in the pinned snapshot, but cannot trigger arbitrary URLs, commands,
provider-time retrieval, or secret access. Onboarding does not clone or execute
the source repository.

The provider first returns structured evidence with internal source references,
then revises that validated object once against the quality rubric. Onboarding
renders the Markdown itself. Invalid extraction falls back to the TODO template. Revision failure uses the
first validated evidence. Safe warnings identify the failed stage, failure
category, fallback, and operator action without printing the wrapped provider
error. After an interactive API failure, choose whether to retry the same
reviewed provider, continue with the TODO template, or cancel. The safe default
is to continue with the template.

The final review labels the prompt as `TODO template`, `Experimental API draft`,
or `TODO template after experimental API failure`. The final write confirmation
defaults to no.

Press `Ctrl+C`, send EOF, or answer no at the final confirmation to leave the
filesystem unchanged.

## Preview with a dry run

A dry run performs discovery, the final job sweep, rendering, output-path
checks, and strict configuration validation without writing files or opening a
pull request:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -source-repo owner/source \
  -dry-run
```

Use `--no-prompt` when you want the reviewable TODO template instead of
sending source evidence and matched Prow metadata to an AI provider. This flag
controls prompt drafting. It does not disable the interactive wizard.

Use `--prompt-debug` for sanitized diagnostics on stderr. Debug output contains
stage timing, bounded source paths and line ranges, counts, provider hostname,
model fingerprint, safe HTTP metadata, and validation codes. It never creates a
report file and excludes tokens, source contents, prompts, model responses,
provider bodies, endpoint paths or query strings, and full model identifiers.

Automation can add `--require-prompt-draft` to fail before local writes or pull
request creation unless the experimental API draft succeeds. The flag is valid
only for experimental API drafting and requires `AI_TOKEN`, `AI_ENDPOINT`, and
`AI_MODEL`.

## Next steps

After the scaffold is written:

1. Review `project.yaml` and `prompts/system.md`.
2. Follow `CHECKLIST.md` for Pages or `deploy/README.md` for Kubernetes.
3. Run the read-only doctor before deployment:

   ```bash
   go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest \
     onboard doctor \
     -project-dir ./my-dashboard
   ```

4. Deploy the smallest working configuration first.
5. Confirm that the expected jobs appear before enabling optional automation.

Do not add notifications, issue automation, fix generation, source
investigation, or Orka until the first fetch publishes the expected dashboard.

## More detail

Use the [onboarding reference](onboarding-reference.md) for:

- Read-only discovery output and ranking behavior.
- Accepted repository forms.
- Fully flagged and non-interactive automation.
- Opening a scaffold pull request.
- AI prompt drafting and inference limits.
- Doctor checks and the complete command surface.

Deployment references:

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes quickstart](kubernetes.md)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)
