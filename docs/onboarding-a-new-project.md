# Onboarding a project

This is the guided quickstart for a first deployment. For every flag, discovery
rule, prompt-authoring mode, update behavior, and automation contract, use the
[complete onboarding reference](onboarding-reference.md).

## Choose an onboarding method

Every method creates the same small consumer configuration and can target either
GitHub Pages or Kubernetes. Choose the workflow that best matches how you want
to make and review the files.

| Method | Best for | Start here |
| --- | --- | --- |
| Interactive wizard | A first setup where you want the CLI to discover jobs and walk through each decision | [Run the interactive wizard](#interactive-wizard) |
| Coding agent-assisted | A conversational setup where a coding agent runs the reviewed discovery, plan, apply, and handoff workflow | [Use a coding agent](#coding-agent-assisted-onboarding) or open the [complete agent guide](agent-onboarding.md) |
| Non-interactive CLI | Scripts, repeatable automation, or operators who already know the required inputs | [Use the flagged CLI](#non-interactive-cli-onboarding) |
| Manual setup | Full control over each consumer file without using the scaffold generator | [Create the files manually](#manual-setup) |

## What onboarding creates

The wizard, coding-agent, and non-interactive paths use `fetcher onboard` to
discover jobs, validate the result, and create a small consumer repository.
Manual setup creates the same file contract directly. In every case, the
consumer points at the shared engine instead of copying engine code.

The common files are:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml   # GitHub Pages
# or
deploy/values.yaml             # Kubernetes
```

The generated scaffold also includes a short deployment guide. Pages receives
`CHECKLIST.md`. Kubernetes receives `deploy/README.md`. Prompt handoff mode may
add agent instructions for completing `prompts/system.md`.

## Interactive wizard

From the source repository checkout:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard
```

The wizard detects the current GitHub `origin` where possible. It then walks you
through:

1. Prow or TestGrid discovery.
2. GitHub Pages or Kubernetes deployment.
3. Project identity and dashboard destination.
4. AI provider and prompt choices.
5. The output directory or pull request target.

For a private repository, export `GITHUB_TOKEN` before starting. The token is
used for GitHub API reads and is not written to the scaffold.

Review the final plan before confirming. The default final answer is no, and
cancellation leaves the filesystem unchanged.

## Coding agent-assisted onboarding

Use a coding agent when you want the same engine-owned discovery and plan/apply
workflow with conversational help resolving inputs, reviewing changes, and
preparing the diagnostic-authoring handoff. The agent does not replace the
engine CLI or hand-write scaffold files.

Install the portable setup and diagnostic-authoring skills:

```bash
npx --yes skills@latest add willie-yao/prow-ai-dashboard \
  --skill setup-prow-ai-consumer author-prow-ai-diagnostics \
  --agent codex \
  --global \
  --yes
```

Then ask the agent to use `$setup-prow-ai-consumer`, for example:

```text
Use $setup-prow-ai-consumer to create a Pages consumer for
https://github.com/kubernetes-sigs/kueue.
```

The setup skill runs read-only discovery, reviews an exact dry-run plan,
preserves consumer-owned files during updates, applies only the reviewed plan,
and validates the resulting consumer. It leaves template placeholders in the
source-only prompt for review. After setup, `$author-prow-ai-diagnostics` can improve
the prompt from historical failures and propose inactive diagnostic recipes.

See [Agent-driven setup and diagnostic authoring](agent-onboarding.md) for
installation scopes, example requests, safety boundaries, update behavior, and
the complete setup-to-authoring workflow.

## Non-interactive CLI onboarding

Use the flagged CLI when the source, discovery selector, consumer repository,
deployment mode, and destination are already known. This is the direct path for
scripts and repeatable automation without the interactive wizard or a coding
agent.

Start with a dry run and save the exact reviewed plan before applying it. See
[Non-interactive automation](onboarding-reference.md#non-interactive-automation)
and [Dry-run behavior](onboarding-reference.md#dry-run-behavior) for the required
flags, plan digest, update safeguards, and machine-readable handoff outputs.

## Review the generated files

Before deployment, check:

- `project.yaml`: source repository, TestGrid dashboard or bucket, storage,
  branding, and inferred job categories.
- `prompts/system.md`: every project-specific architecture, artifact, failure,
  and transient-classification claim.
- `.github/workflows/deploy.yml` or `deploy/values.yaml`: provider coordinates,
  credentials, persistence, and deployment-specific paths.
- `CHECKLIST.md` or `deploy/README.md`: the remaining setup commands.

Generated prompts and categories are drafts. Keep unresolved details explicit
instead of accepting plausible guesses. Do not commit provider tokens or other
Secrets.

## Choose Pages or Kubernetes

| Deployment | Choose it when | First deployment guide |
| --- | --- | --- |
| GitHub Pages | The dashboard is public and read-only, and artifacts and the model endpoint are reachable from the runner | [GitHub Actions and Pages](github-pages.md) |
| Kubernetes | The model endpoint is private to the cluster, data needs shared persistence, or you want authenticated server features | [Kubernetes quickstart](kubernetes.md) |

Both paths use in-process analysis. New users do not need to choose or install an
external runtime.

Start with the smallest working deployment. Add authenticated chat, File Issue,
Mark Resolved, notifications, or other optional features only after the expected
jobs and analyses are visible. See [Optional features](optional-features.md).

## Manual setup

Manual setup is a supported alternative when you prefer to create the consumer
files yourself.

Create this minimal structure:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml   # GitHub Pages
# or
deploy/values.yaml             # Kubernetes
```

Use these references:

- [`configs/example/project.yaml`](../configs/example/project.yaml) for the
  minimal required project fields.
- [`configs/example/prompts/system.md`](../configs/example/prompts/system.md) for
  the prompt file shape.
- [Project configuration](project-configuration.md) for the strict schema.
- [GitHub Actions and Pages](github-pages.md) for the reusable workflow.
- [Kubernetes quickstart](kubernetes.md) for consumer values and the deployment
  wrapper.
- [CAPZ Prow AI Dashboard](https://github.com/willie-yao/capz-prow-ai-dashboard)
  for a current public Pages consumer.

`configs/example` is documentation-only. It contains placeholders and is not a
ready-to-deploy project configuration. Replace the project identity, discovery,
storage, branding, prompt, and deployment settings for your repository.

When the files are ready, run:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest \
  onboard doctor \
  -project-dir ./my-dashboard
```

## Validate with `onboard doctor`

Run the same doctor command after any onboarding method. It checks:

- Strict `project.yaml` parsing.
- A non-empty `prompts/system.md`.
- Pages workflow or Kubernetes values wiring.
- AI provider coordinates and credential source.
- The real Prow discovery sweep and a nonzero job count.

Doctor is read-only. It does not call the model provider or inspect a Kubernetes
cluster. Fix failures before deploying and review warnings that depend on
external settings.

## Deploy

For Pages, complete `CHECKLIST.md` and follow
[GitHub Actions and Pages](github-pages.md).

For Kubernetes, the platform administrator first follows
[Kubernetes platform setup](kubernetes-platform.md). The project contributor
edits `deploy/values.yaml`, follows the generated `deploy/README.md`, and uses
the [Kubernetes quickstart](kubernetes.md). A published CLI and chart release do
not require an engine source checkout.

A successful first deployment shows the expected branding and jobs, publishes
grounded analysis when AI is enabled, and serves healthy data endpoints. Keep
optional automation disabled until that baseline works.

## Complete reference

Use the [onboarding reference](onboarding-reference.md) for:

- Read-only discovery output and repository resolution.
- Flagged, dry-run, and non-interactive usage.
- Opening or updating a scaffold pull request.
- Prompt-authoring modes, timeouts, and fallback behavior.
- Complete doctor behavior and command contracts.

For coding agent-assisted onboarding, use the
[Coding agent-assisted onboarding](#coding-agent-assisted-onboarding) section or
the [complete agent guide](agent-onboarding.md). For scripts and repeatable
automation, use the [non-interactive CLI section](#non-interactive-cli-onboarding).

The [documentation map](README.md) links the remaining
deployment, analysis, operator, experimental, and contributor references.
