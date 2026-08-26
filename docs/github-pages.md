# Deploying with GitHub Actions and Pages

The Pages path runs the fetcher in GitHub Actions, builds the SPA, and publishes
a static read-only dashboard. It needs no cluster or application server.

Use this path when the AI endpoint is reachable from the selected Actions runner
and you do not need interactive admin actions.

## Prerequisites

- A host repository that does not already publish another Pages site.
- `project.yaml` and `prompts/system.md` in the repository root or one subdirectory.
- When AI is enabled, an OpenAI-compatible Chat Completions or Responses
  endpoint with function calling.
- When AI is enabled, the API selector, endpoint URL, model id, and bearer
  token.

Run the guided [`aster onboard`](onboarding-a-new-project.md) flow to generate
and validate these files. The wizard does not enable Pages or write repository
variables and Secrets. Use `-dry-run` to review the complete plan without
writing files.

You can also create the workflow below manually.

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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.9
    with:
      ai-api: ${{ vars.AI_API }}
      ai-model: ${{ vars.AI_MODEL }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
      ai-reasoning-effort: ${{ vars.AI_REASONING_EFFORT }}
      ai-context-window-tokens: ${{ vars.AI_CONTEXT_WINDOW_TOKENS }}
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
# Optional. Empty or unset uses the provider default.
gh variable set AI_REASONING_EFFORT --body high --repo my-org/my-dashboard
# Optional. Set only when independently verified for the selected endpoint.
gh variable set AI_CONTEXT_WINDOW_TOKENS --body 128000 --repo my-org/my-dashboard
gh variable set AI_MODEL --repo my-org/my-dashboard

# Required bearer token. A non-empty placeholder works for an unauthenticated endpoint.
gh secret set AI_TOKEN --repo my-org/my-dashboard
```

The variable and secret commands read values interactively. You may also pass
`--body` for nonsecret variables. Keep the token in `AI_TOKEN`; never put it in
`AI_ENDPOINT`, `project.yaml`, or the workflow.

The provider used to draft `prompts/system.md` during onboarding is a separate
choice. Using it does not configure the deployed Pages workflow.

Validate the local scaffold and workflow structure with:

```bash
aster onboard doctor -project-dir ./my-dashboard
```

Doctor validates the workflow mappings but cannot read the values stored in
GitHub repository variables or Secrets.

## Engine version

The workflow ref controls both the reusable workflow and the engine checkout.
Pin a currently published version exactly:

```yaml
# Current prerelease, pinned exactly.
uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.9
```

After a stable release is published, pin its exact `vMAJOR.MINOR.PATCH` tag.
Commit SHAs are appropriate for engine development. Do not use `@main`,
`@latest`, or a moving major alias as a production version.

The reusable workflow fails closed when GitHub does not provide its resolved
repository, ref, or commit SHA. After checkout it verifies that the engine HEAD
matches that commit. Every published site includes `data/provenance.json` with
the caller commit, reusable-workflow commit, and engine commit. TestGrid
consumers also include the effective test-infra revision when `manifest.json`
reports one.

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
- `/data/provenance.json` reports matching reusable-workflow and engine commits,
  plus the effective test-infra revision for TestGrid discovery.
- `/data/dashboard.json` contains the discovered jobs.
- A failed test in `/data/jobs/*.json` contains grounded AI analysis.

See [Troubleshooting](troubleshooting.md) if the workflow succeeds but the site
is empty or analysis is unavailable.

## Intentional AI cache rebaseline

Provider, model, prompt, and skill changes affect new analyses but do not
invalidate existing reusable entries. Use the reusable workflow's
`ai-cache-generation` input for a reversible full rebaseline:

```yaml
with:
  ai-cache-generation: "2"
```

Generation `2` misses generation `1`; returning to `1` reuses its unexpired
entries. Empty preserves all historical keys byte-for-byte. The value is not
published in dashboard JSON. Keep `.github/workflows/reusable-clear-cache.yml`
for emergency destructive cleanup only.

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
  ./bin/aster -project-dir=<project_dir> -out=<project_dir>/data -ai

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
    uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.9
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

## A Pages deployment performs no maintainer-initiated GitHub writes

A static Pages site has no authenticated action API, so the guarded dashboard
actions are all [Kubernetes-native](kubernetes.md) server features: interactive
chat, File Issue, Mark Resolved, and [Fix PR generation](fix-prs.md), which
additionally requires the Agent Sandbox runtime. Of those, File Issue and Fix PR
generation are the GitHub writes; chat is read-only and Mark Resolved updates
local state. Setting `issues.enabled` or `ai.fix_prs.enabled` in a Pages
consumer's `project.yaml` has nothing to act on.

One unattended write does run here, because it happens in the fetch step rather
than the server: the optional bot comment on newly opened pull requests, which
posts only when `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` are supplied as
secrets. It is off by default and stays in dry run until `dry_run` is explicitly
false. See
[Pull request triage](pull-request-triage.md#optional-bot-comment).
