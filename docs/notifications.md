# Slack notifications

The fetcher can send Slack alerts for persistent test failures through an
incoming webhook. Notifications are optional and require no `project.yaml`
fields.

## Enable notifications

For GitHub Actions and Pages, pass the secret to the reusable workflow:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
```

Create the secret with:

```bash
gh secret set SLACK_WEBHOOK_URL --repo my-org/my-dashboard
```

For Kubernetes-native deployments, inject `SLACK_WEBHOOK_URL` through
`fetcher.extraEnv` from a Secret.

## Alert behavior

A test enters the persistent set after at least three consecutive failures. The
notifier sends:

- One alert when a persistent failure first appears.
- Another alert if the failure's error hash changes.
- No repeated alert while the same error continues.
- A recovery message when the test leaves the persistent set.

Failure alerts include the job, test, consecutive count, latest error, available
AI root cause, dashboard link, and Prow link.

Deduplication state is stored in `notification_state.json` alongside the fetcher
cache. It is preserved between runs but is not published by Pages or served by
the Kubernetes server.

## Failure behavior

Webhook errors are logged and do not fail the fetch. Transport errors redact the
webhook URL because its path contains the secret.

Only Slack incoming webhooks are implemented. Microsoft Teams is not currently
supported.
