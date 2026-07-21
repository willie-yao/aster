# Email notifications

The fetcher can send SMTP email alerts for persistent test failures, changed
failure modes, and recoveries. Notifications are optional and run during the
full fetch pass or the watch worker's reconcile pass.

## Alert behavior

A test enters the persistent set after at least three consecutive failures. The
notifier sends:

- One email when a persistent failure first appears.
- Another email if the latest error hash changes.
- No repeated email while the same error continues.
- A recovery email when the test leaves the persistent set.

Failure emails include the project, job, test, consecutive count, latest error,
available AI root cause or summary, dashboard link, and Prow link. Each message
contains plain-text and HTML alternatives.

When `action_links: true`, the notifier also sends pattern-level email. It sends
one message when a job first becomes systemic and another when that job's shared
root cause changes materially. Ordinary AI paraphrasing updates the stored
pattern without sending again. A changed-pattern message includes both the
previous and current root causes. Each message includes inert **Review issue
draft** and **Review fix proposal** links.

## Project configuration

Add the email block to the consumer's `project.yaml`:

```yaml
notifications:
  email:
    enabled: true
    action_links: false
    from: "Prow AI Dashboard <prow-dashboard@example.com>"
    to:
      - "ci-team@example.com"
    smtp:
      host: "smtp.example.com"
      port: 587
      username: "prow-dashboard@example.com"
      tls: starttls
```

The consumer config is strict. When email is enabled, `from`, at least one `to`
recipient, and `smtp.host` are required. Sender and recipient values must be
valid email addresses.

Use a team distribution list rather than personal addresses when `project.yaml`
is stored in a public repository. Notification settings are omitted from the
published `manifest.json`, but the source YAML itself remains visible.

## Action links for maintainers

Action links are opt-in and only work with a Kubernetes-native server that has
admin actions enabled:

```yaml
notifications:
  email:
    enabled: true
    action_links: true
    # from, to, and smtp omitted here
```

The links contain only the public pattern id, requested action, and dashboard
URL. They contain no GitHub token, SMTP password, preview token, or authorization
data.

Opening a link performs an inert GET. After GitHub OAuth or proxy authentication,
the dashboard shows a second **Generate draft** prompt. Only that explicit click
creates a persistent asynchronous request. The user can leave the page while the
server generates the draft. When server-side SMTP is configured, the dashboard
emails a draft-ready review link. The requesting maintainer signs in, reviews the
exact issue or diff, and confirms it before the dashboard writes to GitHub.

This extra step is intentional. Email security scanners and forwarded messages
must not be able to start generation or create an issue or PR merely by opening
a URL.

Do not enable `action_links` for a static Pages deployment. Pages has no action
API or administrator authentication. Fix proposal links also require a server
image that contains `opencode` and git.

## SMTP security modes

`smtp.tls` accepts:

| Value | Behavior | Default port |
| --- | --- | --- |
| `starttls` | Connect in plaintext, require STARTTLS, then authenticate and send over TLS. This is the default. | 587 |
| `tls` | Establish implicit TLS before speaking SMTP. | 465 |
| `none` | Use an unauthenticated plaintext relay. This must be selected explicitly. | 25 |

Authenticated SMTP is rejected when `tls: none` so credentials are never sent
over plaintext. TLS validates the relay certificate and requires TLS 1.2 or
newer. There is no insecure certificate-skip option.

## GitHub Actions and Pages

When `smtp.username` is configured, pass the password through the reusable
workflow:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      EMAIL_SMTP_PASSWORD: ${{ secrets.EMAIL_SMTP_PASSWORD }}
```

Create the secret once:

```bash
gh secret set EMAIL_SMTP_PASSWORD --repo my-org/my-dashboard
```

The SMTP relay must be reachable from the selected runner. For a private relay,
use a self-hosted runner, the Kubernetes-native deployment, or an externally
reachable trusted relay.

An unauthenticated relay leaves `smtp.username` empty and does not need the
password secret.

## Kubernetes-native deployment

Store the password in a Kubernetes Secret. Expose it to the fetcher or worker
for scheduled alerts, and to the server for asynchronous draft-ready emails:

```bash
kubectl -n dashboards create secret generic capz-smtp \
  --from-literal=password=<smtp-password>
```

```yaml
fetcher:
  extraEnv: &smtpEnv
    - name: EMAIL_SMTP_PASSWORD
      valueFrom:
        secretKeyRef:
          name: capz-smtp
          key: password
server:
  extraEnv: *smtpEnv
```

The fetcher sends scheduled failure alerts. The server sends draft-ready emails
for asynchronous action requests. Ready requests and their exact reviewed drafts
are persisted in non-public `action_request_state.json` for 24 hours. Pending
requests interrupted by a server restart are marked failed and can be recreated.

## State and retry behavior

Deduplication state is stored in `notification_state.json` alongside the fetcher
cache. It is preserved between runs but is not published by Pages or served by
the Kubernetes server.

State tracks both persistent-test alerts and action-enabled systemic-pattern
alerts. Systemic patterns are keyed by job ID, with the latest pattern ID and
root cause retained for links and changed-pattern comparison. The state resets
after an authoritative non-systemic verdict or recovery. Delivery-tracking
transitions happen only after successful delivery; ordinary paraphrases may
refresh the stored pattern metadata without sending a message:

- A failed new or changed-failure email is retried on the next full pass.
- A failed new or changed-pattern email is retried on the next full pass.
- A failed recovery email keeps its state entry and is retried.
- Delivery failures are logged and do not fail the fetch or block other side
  effects.

Remediation lifecycle emails are deduplicated separately in
`remediation_state.json`. They are sent only when a tracked pull request changes
state, such as waiting for a presubmit, passing pre-merge verification, entering
post-merge observation, being verified fixed, or reproducing the same failure.
A same-cause recurrence links to the remediation status and prior pull request.
Failed delivery does not advance the emailed transition, so the next finalized
pass retries it.

The email implementation uses the state channel `email-v1`. On the first run
after upgrading from Slack notifications, old channel-less state is reset. Each
currently persistent failure therefore receives one initial email.

## Troubleshooting

- **`email disabled` in logs:** add `notifications.email.enabled: true`.
- **`EMAIL_SMTP_PASSWORD is unset`:** set the secret, or remove `smtp.username`
  when using an unauthenticated relay.
- **Relay does not advertise STARTTLS:** configure the relay for STARTTLS or use
  the correct `tls` mode. The engine does not downgrade automatically.
- **Certificate error:** use a certificate trusted by the runner or pod. The
  engine does not support insecure certificate skipping.
- **Connection timeout:** verify DNS, firewall rules, port, and reachability from
  the execution environment.
- **Repeated attempts after an error:** expected behavior. Failed deliveries do
  not advance notification state.

## Implementation reference

- `backend/internal/notify/notify.go`: triggers, deduplication, recovery, state.
- `backend/internal/notify/email.go`: failure and recovery email rendering.
- `backend/internal/notify/remediation.go`: remediation transition rendering.
- `backend/internal/notify/smtp.go`: SMTP, authentication, TLS, MIME delivery.
- `backend/internal/fetcher/fetcher.go`: configuration and side-effect wiring.
