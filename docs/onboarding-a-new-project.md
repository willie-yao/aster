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
7. Whether to draft `prompts/system.md` from bounded repository documentation.
8. The output directory or pull request destination.

In the interactive form, use the arrow keys to move, Enter to select, and
`Ctrl+C` to cancel. Inferred text values are prefilled and editable. The final
confirmation defaults to no.

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

Repository metadata, Prow configuration, and source documentation are untrusted
input. They cannot alter the wizard flow or cause command execution. Repository
content is sent to a prompt-drafting provider only after explicit confirmation.

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

Use `-no-prompt` when you want the reviewable prompt stub instead of sending
repository documentation to an AI provider. This flag controls prompt drafting.
It does not disable the interactive wizard.

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
