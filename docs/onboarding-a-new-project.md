# Onboarding a project

`fetcher onboard` creates a validated consumer scaffold for GitHub Pages or
Kubernetes. Start with the guided wizard, review the generated files, then
follow the deployment guide included in the scaffold.

An agent can run the same discovery, dry-run, scaffold, prompt handoff, and
doctor flow with the repo-owned
[`setup-prow-ai-consumer` skill](agent-onboarding.md). The skill uses the CLI as
the scaffold authority rather than maintaining separate templates.

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
7. Whether to generate `prompts/system.md` with OpenCode, write an agent
   handoff bundle, or keep the TODO template.
8. The output directory or pull request destination.

In the interactive form, use the arrow keys to move, Enter to select, and
`Ctrl+C` to cancel. Press Esc to clear the current prefilled text input without
submitting it. Inferred text values remain editable. The final confirmation
defaults to no.

The dashboard repository must be a consumer repository you control. The wizard
prefers the authenticated GitHub login, then the owner of an automatically
detected Git remote. Selecting an upstream fork source for Prow discovery does
not change that destination owner. If no safe owner is known, enter `owner/name`
explicitly. The optional short name starts empty because repository initials do
not reliably identify established project abbreviations.

When the source comes from the current Git checkout, the local destination
defaults to a sibling such as `../<dashboard-repository-name>`. This keeps the
source and dashboard consumer repositories separate. When the source was
provided explicitly, the default is a relative directory under the current
working directory. `--out` always overrides the default.

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

Handoff mode also includes `PROMPT_HANDOFF.md` and
`.opencode/skills/system-prompt-generation/SKILL.md`.

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

Handoff mode also includes `PROMPT_HANDOFF.md` and
`.opencode/skills/system-prompt-generation/SKILL.md`.

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

Repository metadata, Prow configuration, source files, and job metadata are
untrusted input. They cannot alter the wizard flow. Agent handoff metadata is
serialized as data and tells the agent not to treat any field as an instruction.

Agent mode resolves the source branch to an immutable commit, creates a
temporary shallow checkout and OpenCode config, and runs the local OpenCode
process through the pinned `srt` OS sandbox with its shell tool disabled. The
runtime accepts the result only when the agent changes exactly
`prompts/system.md` and the file passes deterministic structure and quality
validation. It uses the selected provider credential from the user's existing
OpenCode configuration. `AI_TOKEN` is not required for this mode. Install the pinned
`srt` package and set `SRT_BIN` as described in
[Local OpenCode sandbox](local-opencode-sandbox.md). GitHub Copilot domains are
built in; other providers use repeated `--prompt-network-domain` flags.

Agent failures fall back to the TODO template and handoff bundle. Safe warnings
identify source-resolution, execution, timeout, or output-validation failures
without printing raw OpenCode output. The final review shows the requested mode,
agent model and timeout where applicable, final prompt status, and safe fallback
diagnostics. The final write confirmation defaults to no.

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

Use `--no-prompt` when you want only the reviewable TODO template. This flag
controls prompt authoring. It does not disable the interactive wizard.

Local onboarding refuses to replace generated files unless `--update-existing`
is explicit. Interactive onboarding instead offers another directory, updating
only the listed scaffold files, or cancellation. The safe default is another
directory. Before confirmation, review every file marked `create` or `replace`.
Unrelated files and stale generated files from another deployment or prompt mode
are left untouched.
Open-PR mode continues to use a GitHub diff and does not use local update mode.

Automation can add `--require-prompt-draft` to fail before local writes or pull
request creation unless agent drafting succeeds. It is valid only with
`--prompt-mode=agent` and uses OpenCode ambient authentication.

Prompt preparation has a 15-minute total timeout by default. Slow agent runs can
use `--prompt-timeout`, for example `--prompt-timeout 30m`. The accepted range is
one minute through two hours. This timeout covers source revision resolution and
agent execution. It is separate from the normal fetcher timeout and deployed
`ai.timeout`.

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
- Prompt authoring modes and safety limits.
- Doctor checks and the complete command surface.

Deployment references:

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes quickstart](kubernetes.md)
- [Project configuration](project-configuration.md)
- [Troubleshooting](troubleshooting.md)

### Prompt authoring modes

The wizard can generate `prompts/system.md` with a local OpenCode agent in a
temporary checkout, write a reusable agent handoff bundle, or write the TODO
template. Agent mode defaults to
`github-copilot/claude-sonnet-4.6` and uses the selected provider credential from
the user's existing OpenCode configuration. Handoff mode writes
`PROMPT_HANDOFF.md` and `.opencode/skills/system-prompt-generation/SKILL.md`
without running an agent. `--no-prompt` is equivalent to
`--prompt-mode=todo-template` and cannot be combined with another explicit
prompt mode. The handoff records a pinned commit when GitHub resolution
succeeds. If source resolution is unavailable, it preserves the known default
branch or marks the ref unresolved instead of guessing one.
