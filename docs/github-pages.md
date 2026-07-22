# Deploying with GitHub Actions and Pages

The Pages path runs the fetcher in GitHub Actions, builds the SPA, and publishes
a static read-only dashboard. It needs no cluster or application server.

Use this path when the AI endpoint is reachable from the selected Actions runner
and you do not need interactive admin actions.

## Prerequisites

- A host repository that does not already publish another Pages site.
- `project.yaml` and `prompts/system.md` in the repository root or one subdirectory.
- An OpenAI-compatible Chat Completions or Responses endpoint with function calling.
- The API selector, endpoint URL, model id, and bearer token.

Run [`fetcher onboard`](onboarding-a-new-project.md) to generate the files, or
create the workflow below manually.

## Deploy workflow

```yaml
name: Deploy Dashboard

on:
  schedule:
    - cron: "*/30 * * * *"
  workflow_dispatch: {}
  push:
    branches: [main]

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: deploy
  cancel-in-progress: false

jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      ai-api: ${{ vars.AI_API }}
      ai-model: ${{ vars.AI_MODEL }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

If `project.yaml` already sets `ai.model` and `ai.endpoint`, those values take
precedence and the two repository variables may be omitted.

Set `project_dir` to a subdirectory such as `dashboard` when the consumer files
are not at the repository root.

## Repository configuration

```bash
# Enable Pages with the GitHub Actions build source.
gh api repos/my-org/my-dashboard/pages -X POST -F build_type=workflow

# Required unless project.yaml contains ai.endpoint and ai.model.
gh variable set AI_API --body chat_completions --repo my-org/my-dashboard
gh variable set AI_ENDPOINT --repo my-org/my-dashboard
gh variable set AI_MODEL --repo my-org/my-dashboard

# Required bearer token. A non-empty placeholder works for an unauthenticated endpoint.
gh secret set AI_TOKEN --repo my-org/my-dashboard
```

The variable commands read the value interactively. You may also pass `--body`.

## Engine version

The workflow ref controls both the reusable workflow and the engine checkout.
Use only refs that currently exist:

```yaml
# Current development version.
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main

# Existing prerelease or release, pinned exactly.
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1.0.0-beta.5
```

The moving `@v1` alias is created only when the first stable `v1.0.0` release is
published. Do not use `@v1` before that alias exists.

## Host repository layout

A dedicated repository puts the files at the root:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml
```

An existing repository can use a subdirectory:

```text
dashboard/project.yaml
dashboard/prompts/system.md
.github/workflows/deploy.yml
```

Set `project_dir: dashboard` in the deploy workflow. A repository can publish only one
Pages site. If it already has one, use a dedicated dashboard repository or the
Kubernetes-native deployment.

## First deploy

```bash
gh workflow run deploy.yml --repo my-org/my-dashboard
gh run watch --repo my-org/my-dashboard --exit-status
```

After the run succeeds, check:

- The Pages root returns the dashboard.
- `/data/manifest.json` has the expected branding.
- `/data/dashboard.json` contains the discovered jobs.
- A failed test in `/data/jobs/*.json` contains grounded AI analysis.

See [Troubleshooting](troubleshooting.md) if the workflow succeeds but the site
is empty or analysis is unavailable.

## Manual full cache reset

Provider, model, prompt, and skill changes invalidate affected analyses
automatically. A clear-cache workflow is therefore not generated for new
projects. If an operator needs an immediate full rebaseline, add a small workflow
that calls `.github/workflows/reusable-clear-cache.yml` with the same engine ref
and `project_dir` as the deploy workflow.

## Private AI endpoints

GitHub-hosted runners cannot reach a ClusterIP or private network endpoint. Use
one of these options:

1. Choose the [Kubernetes-native deployment](kubernetes.md).
2. Set the reusable workflow's `runs-on` input to a preconfigured self-hosted
   runner that can reach the endpoint.
3. Fetch elsewhere, commit `<project_dir>/data`, and set `skip-fetch: true`.

For pre-fetched data:

```bash
AI_ENDPOINT="http://localhost:8000/v1/chat/completions" \
AI_MODEL="model-id" AI_TOKEN="token-or-placeholder" \
  ./bin/fetcher -project-dir=<project_dir> -out=<project_dir>/data -ai

git add <project_dir>/data
git commit -m "Refresh prefetched data"
git push
```

Operational cache and write-state files are removed before Pages publication.

## Optional features

### Email notifications

Enable `notifications.email` in `project.yaml`, then pass the SMTP password when
the relay uses authentication:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      EMAIL_SMTP_PASSWORD: ${{ secrets.EMAIL_SMTP_PASSWORD }}
```

```bash
gh secret set EMAIL_SMTP_PASSWORD --repo my-org/my-dashboard
```

The SMTP host must be reachable from the selected runner. Keep
`notifications.email.action_links` false because a static Pages deployment has
no authenticated action API. See [Email notifications](notifications.md) for the
project configuration and TLS modes.


Email notifications, automatic issues, and scheduled fix PRs run during the
fetch step when configured. Interactive actions are not available on a static
Pages site.

Scheduled fix PR generation also requires `opencode` and git on the runner. The
standard GitHub-hosted workflow does not install `opencode`, so use a
preconfigured self-hosted runner before enabling that feature.
