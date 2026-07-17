---
goal: Replace Slack webhook alerts with SMTP email notifications
version: 1.0
date_created: 2026-07-17
last_updated: 2026-07-17
owner: prow-ai-dashboard maintainers
status: 'Completed'
tags: [feature, notifications, email, smtp, breaking-change]
---

# Introduction

![Status: Completed](https://img.shields.io/badge/status-Completed-brightgreen)

Replace the Slack incoming-webhook notification transport with SMTP email while preserving the existing persistent-failure, changed-error, recovery, deduplication, and best-effort side-effect behavior. The implementation uses the Go standard library, stores non-secret delivery settings in `project.yaml`, accepts the SMTP password through `EMAIL_SMTP_PASSWORD`, and removes `SLACK_WEBHOOK_URL` from all supported deployment paths.

The initial email implementation sends one message per notification event. Digesting or batching is outside this plan because it changes the current delivery semantics and requires separate product decisions about cadence and grouping.

## 1. Requirements & Constraints

- **REQ-001**: Remove all Slack-specific production code, workflow secrets, scaffold output, documentation, examples, and comments.
- **REQ-002**: Preserve the current notification trigger threshold: notify only entries in `FlakinessReport.PersistentFailures` whose `ConsecutiveFailures` is at least 3.
- **REQ-003**: Send an email when a persistent failure is first observed.
- **REQ-004**: Send another email when the persistent failure's latest `ErrorHash` changes.
- **REQ-005**: Send a recovery email when a previously notified failure is absent from the current persistent-failure set.
- **REQ-006**: Preserve the deduplication key format `JobID + "::" + TestName` so presubmit and periodic jobs with the same display name do not collide.
- **REQ-007**: Include the project name, test name, job name, consecutive-failure count, truncated failure message, newest available AI root cause or summary, dashboard URL, and Prow URL in failure emails.
- **REQ-008**: Include the project name, test name, job name, prior consecutive-failure count, and dashboard URL in recovery emails.
- **REQ-009**: Produce both plain-text and HTML MIME alternatives in every email.
- **REQ-010**: Send one message to all configured recipients through the SMTP envelope and `To` header.
- **REQ-011**: Keep `notification_state.json` as the operational state filename so output filtering and volume layout remain stable.
- **REQ-012**: Add `channel: "email-v1"` to `NotificationState`. Treat state with an empty or different channel as fresh state. This intentionally sends one initial email for every currently persistent failure after migration from Slack.
- **REQ-013**: Update notification state only after successful delivery. A failed new or changed-failure email must be retried on the next full pass. A failed recovery email must retain its state entry and be retried on the next full pass.
- **REQ-014**: Continue processing other notification events after one email fails. Return aggregate statistics and an aggregate error after all events have been attempted.
- **REQ-015**: Add `Stats.Failed` and log sent failure alerts, sent recoveries, and failed deliveries separately.
- **REQ-016**: Configure email through the following strict `project.yaml` shape:

  ```yaml
  notifications:
    email:
      enabled: true
      from: "Prow AI Dashboard <prow-dashboard@example.com>"
      to:
        - "ci-team@example.com"
      smtp:
        host: "smtp.example.com"
        port: 587
        username: "prow-dashboard@example.com"
        tls: starttls
  ```

- **REQ-017**: Add these project configuration types:
  - `project.Config.Notifications *Notifications` with `json:"-"`.
  - `Notifications.Email *EmailNotifications` with `json:"-"`.
  - `EmailNotifications.Enabled bool`.
  - `EmailNotifications.From string`.
  - `EmailNotifications.To []string`.
  - `EmailNotifications.SMTP EmailSMTP`.
  - `EmailSMTP.Host string`.
  - `EmailSMTP.Port int`.
  - `EmailSMTP.Username string`.
  - `EmailSMTP.TLS string`.
- **REQ-018**: Apply these email defaults through `EffectiveEmailNotifications`:
  - `smtp.tls` defaults to `starttls`.
  - `smtp.port` defaults to 587 for `starttls`, 465 for `tls`, and 25 for `none`.
- **REQ-019**: When email notifications are enabled, project validation must require `from`, at least one `to` address, and `smtp.host`.
- **REQ-020**: Project validation must parse `from` and every `to` entry with `net/mail.ParseAddress` and reject malformed addresses.
- **REQ-021**: Project validation must accept only `starttls`, `tls`, or `none` for `smtp.tls` and must reject ports outside 1 through 65535 after defaults are applied.
- **REQ-022**: If `smtp.username` is empty, support an unauthenticated SMTP relay and do not require `EMAIL_SMTP_PASSWORD`.
- **REQ-023**: If `smtp.username` is non-empty and `EMAIL_SMTP_PASSWORD` is empty, skip notifications with one clear log message and do not mutate notification state.
- **REQ-024**: Use SMTP PLAIN authentication when `smtp.username` is non-empty.
- **REQ-025**: Remove `SLACK_WEBHOOK_URL` and add optional `EMAIL_SMTP_PASSWORD` to `.github/workflows/reusable-deploy.yml`.
- **REQ-026**: Pass `EMAIL_SMTP_PASSWORD` only as a secret environment variable. Never place it in `project.yaml`, generated JSON, logs, command arguments, Helm values examples, or email content.
- **REQ-027**: Kubernetes-native deployments must source `EMAIL_SMTP_PASSWORD` through `fetcher.extraEnv` from a Kubernetes Secret. No dedicated chart secret abstraction is added in this change.
- **REQ-028**: Update generated Pages scaffolds to include a commented `notifications.email` example and a commented `EMAIL_SMTP_PASSWORD` secret mapping.
- **REQ-029**: Update generated onboarding checklists with exact commands for setting `EMAIL_SMTP_PASSWORD` after the user enables authenticated SMTP.
- **REQ-030**: Replace the current Slack-focused `docs/notifications.md` with an email guide that documents triggers, delivery semantics, SMTP configuration, GitHub Actions setup, Kubernetes setup, migration, security, and troubleshooting.
- **REQ-031**: Add an `[Unreleased]` changelog entry that identifies removal of `SLACK_WEBHOOK_URL` as a breaking workflow contract and documents the replacement.
- **SEC-001**: Default to encrypted SMTP through STARTTLS. Plain SMTP requires the explicit value `smtp.tls: none`.
- **SEC-002**: For `starttls`, fail delivery if the SMTP server does not advertise STARTTLS. Do not silently downgrade to plaintext.
- **SEC-003**: For implicit TLS, use `tls.Config{ServerName: smtp.host, MinVersion: tls.VersionTLS12}` and validate the server certificate.
- **SEC-004**: For STARTTLS, use the same minimum TLS version and certificate validation. Reject authenticated SMTP when `smtp.tls` is `none` so credentials are never sent over plaintext.
- **SEC-005**: Do not add an insecure certificate-skip option.
- **SEC-006**: Prevent email header injection. Reject or strip CR and LF from every generated header value, and construct address headers from parsed `mail.Address` values.
- **SEC-007**: Escape all test names, job names, failure text, and AI text before inserting them into HTML.
- **SEC-008**: Bound message content with the existing `textutil.Truncate` helper. Keep the existing 200-character failure-message limit and 500-character AI-text limit unless a test demonstrates a MIME encoding issue.
- **SEC-009**: Apply connection and I/O deadlines derived from the caller context. Use a 15-second fallback deadline when the context has no earlier deadline.
- **SEC-010**: Never log the SMTP password. SMTP errors must identify the host without including credentials.
- **CON-001**: Use only the Go standard library for SMTP and MIME. Do not add a new module dependency.
- **CON-002**: Keep notification processing in full passes only. Watch-mode incremental refreshes must not send messages outside the existing reconcile cadence.
- **CON-003**: Keep notifications best-effort. Email delivery failures must not fail the fetch or prevent issue and fix-PR side effects.
- **CON-004**: Do not preserve Slack as a parallel transport or compatibility branch.
- **CON-005**: Do not add digest scheduling, templates supplied by consumers, CC/BCC configuration, OAuth SMTP authentication, attachments, or inbound reply processing.
- **PAT-001**: Use `statefile.WriteJSON` for atomic state persistence instead of direct `os.WriteFile`.
- **PAT-002**: Match existing short comments, wrapped errors, and fetcher log style.
- **PAT-003**: Keep `backend/internal/notify` as the package name because it owns notification policy, state, rendering, and transport.

## 2. Implementation Steps

### Implementation Phase 1

- GOAL-001: Add the email notification configuration contract and validation.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | In `backend/internal/project/project.go`, add `Config.Notifications`, `Notifications`, `EmailNotifications`, and `EmailSMTP` with the exact YAML and JSON tags specified by REQ-017. Keep the complete notifications subtree excluded from `manifest.json` with `json:"-"`. | ✅ | 2026-07-17 |
| TASK-002 | In `backend/internal/project/project.go`, add constants `EmailTLSStartTLS`, `EmailTLSImplicit`, and `EmailTLSNone` with values `starttls`, `tls`, and `none`. | ✅ | 2026-07-17 |
| TASK-003 | Add `(*Config).EffectiveEmailNotifications() (EmailNotifications, bool)`. Return `false` when the block is absent or disabled. When enabled, apply the TLS and port defaults from REQ-018 without mutating the loaded config. | ✅ | 2026-07-17 |
| TASK-004 | Extend `Config.Validate` to enforce REQ-019 through REQ-021. Include indexed recipient errors such as `notifications.email.to[1]` and preserve strict YAML decoding. | ✅ | 2026-07-17 |
| TASK-005 | Add table-driven tests in `backend/internal/project/project_test.go` for disabled configuration, default STARTTLS port, implicit TLS port, plaintext port, explicit port, malformed sender, malformed recipient, empty recipient list, missing host, unsupported TLS mode, and out-of-range port. | ✅ | 2026-07-17 |
| TASK-006 | Extend the manifest serialization test to prove SMTP host, username, sender, recipient addresses, and notification settings are absent from `manifest.json`. | ✅ | 2026-07-17 |
| TASK-007 | Update `configs/example/project.reference.yaml` with a commented complete `notifications.email` block and a note that `EMAIL_SMTP_PASSWORD` is supplied separately. | ✅ | 2026-07-17 |

### Implementation Phase 2

- GOAL-002: Replace Slack payload rendering and HTTP webhooks with transport-independent email events and an SMTP sender.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-008 | Refactor `backend/internal/notify/notify.go` so `Notifier` stores a `Sender` interface instead of `webhookURL` and `http.Client`. Define `Sender.Send(context.Context, Message) error`. Define `Message` with `From mail.Address`, `To []mail.Address`, `Subject`, `TextBody`, and `HTMLBody`. | ✅ | 2026-07-17 |
| TASK-009 | Change `NewNotifier` to accept `Sender`, `stateFile`, `projectName`, `dashboardBaseURL`, and `prowURLBase`. Retain `notificationKey`, AI lookup behavior, and the persistent threshold. | ✅ | 2026-07-17 |
| TASK-010 | Add `NotificationState.Channel string` and constant `notificationChannel = "email-v1"`. In `loadState`, discard state whose channel is empty or different. Initialize fresh state with the current channel. | ✅ | 2026-07-17 |
| TASK-011 | Replace direct state writes in `SaveState` with `statefile.WriteJSON`. | ✅ | 2026-07-17 |
| TASK-012 | Rewrite `ProcessFailures` so new, changed, and recovery state mutations occur only after `Sender.Send` succeeds. Continue after individual failures, increment `Stats.Failed`, and return `errors.Join` of delivery errors after all events are processed. | ✅ | 2026-07-17 |
| TASK-013 | Add an internal notification event type that distinguishes `new failure`, `changed failure`, and `recovery`. Use it only to select the subject and body language. Do not expose it in project configuration. | ✅ | 2026-07-17 |
| TASK-014 | Add `backend/internal/notify/email.go`. Implement pure rendering functions for failure and recovery messages. Subjects must use `[<project>] Persistent failure: <test>`, `[<project>] Failure changed: <test>`, and `[<project>] Recovered: <test>`. | ✅ | 2026-07-17 |
| TASK-015 | Render plain text with stable labeled sections and raw URLs. Render HTML with semantic headings, a details table, escaped preformatted error text, escaped AI analysis, and normal anchor elements for Dashboard and Prow. | ✅ | 2026-07-17 |
| TASK-016 | Delete Slack Block Kit construction, `slackButtons`, webhook JSON payloads, HTTP posting, and Slack-specific imports from `backend/internal/notify/notify.go`. | ✅ | 2026-07-17 |
| TASK-017 | Add `backend/internal/notify/smtp.go`. Implement `SMTPSender` with explicit STARTTLS, implicit TLS, and plaintext connection paths. Use `net.Dialer.DialContext`, `tls.Conn.HandshakeContext`, `net/smtp.Client`, optional `smtp.PlainAuth`, and context-derived deadlines. | ✅ | 2026-07-17 |
| TASK-018 | In `smtp.go`, build an RFC 5322 message with `Date`, `Message-ID`, `From`, `To`, `Subject`, `MIME-Version`, and `multipart/alternative`. Encode non-ASCII subjects with `mime.QEncoding.Encode`. Encode plain and HTML parts with quoted-printable. | ✅ | 2026-07-17 |
| TASK-019 | Add an unexported SMTP client interface and dial function field so tests can assert EHLO, STARTTLS, authentication, envelope sender, recipients, message bytes, and cleanup without a network service. Keep the production constructor simple. | ✅ | 2026-07-17 |
| TASK-020 | Ensure every SMTP failure closes the connection, wraps the operation and host, and excludes the password. | ✅ | 2026-07-17 |

### Implementation Phase 3

- GOAL-003: Wire email delivery into fetcher, Pages, Kubernetes guidance, and generated scaffolds.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-021 | In `backend/internal/fetcher/fetcher.go`, replace `SLACK_WEBHOOK_URL` detection with `cfg.EffectiveEmailNotifications()`. If disabled, log `Notifications: skipped (email disabled)`. | ✅ | 2026-07-17 |
| TASK-022 | In `runSideEffects`, read `EMAIL_SMTP_PASSWORD` only after email is enabled. If `smtp.username` is non-empty and the password is empty, log one skip message and do not construct or run the notifier. | ✅ | 2026-07-17 |
| TASK-023 | Construct `notify.SMTPSender` from effective config and password. On constructor error, log the configuration failure, skip notification processing, and continue with issue and fix-PR side effects. | ✅ | 2026-07-17 |
| TASK-024 | Update fetcher notification logging to include `failure alerts`, `recoveries`, and `failed deliveries`. Log the aggregate processing error after logging the statistics. Always call `SaveState` after processing so successful events persist even when another event failed. | ✅ | 2026-07-17 |
| TASK-025 | In `.github/workflows/reusable-deploy.yml`, remove `SLACK_WEBHOOK_URL`, add optional `EMAIL_SMTP_PASSWORD`, pass it to the fetch step, and update the example caller comment. | ✅ | 2026-07-17 |
| TASK-026 | In `backend/internal/onboard/templates.go`, remove Slack from the generated workflow and checklist. Add commented email configuration to generated `project.yaml`, add the commented `EMAIL_SMTP_PASSWORD` mapping to `deploy.yml`, and add the exact `gh secret set EMAIL_SMTP_PASSWORD` command to `CHECKLIST.md`. | ✅ | 2026-07-17 |
| TASK-027 | Update `backend/internal/onboard/onboard_test.go` assertions for generated Pages and Kubernetes scaffolds. Verify that generated `project.yaml` remains valid while the commented email block is untouched. | ✅ | 2026-07-17 |
| TASK-028 | In `backend/internal/e2e/pipeline_test.go`, clear `EMAIL_SMTP_PASSWORD` instead of `SLACK_WEBHOOK_URL`. Add a fixture test proving disabled email does not attempt delivery or create notification state. | ✅ | 2026-07-17 |
| TASK-029 | Keep `notification_state.json` in `output.NonPublishedFiles`. Update comments to describe email notification state rather than generic webhook state. | ✅ | 2026-07-17 |
| TASK-030 | Remove the stale `skill_suggest_state.json` entry from the reusable workflow's non-published-file removal list while editing that block, because `output.NonPublishedFiles` no longer contains it and the comment says both lists are synchronized. | ✅ | 2026-07-17 |
| TASK-031 | Update `deploy/helm/prow-ai-dashboard/values.yaml` comments to use `EMAIL_SMTP_PASSWORD` as the notification example under `fetcher.extraEnv`. Do not add a dedicated chart value. | ✅ | 2026-07-17 |
| TASK-032 | Update `backend/internal/redact/redact.go` and `redact_test.go` to replace the Slack-specific example with an SMTP gateway or generic secret-bearing URL example. Retain URL redaction behavior unchanged. | ✅ | 2026-07-17 |

### Implementation Phase 4

- GOAL-004: Replace Slack documentation and provide a complete migration path.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-033 | Replace the existing Slack-focused `docs/notifications.md` with sections: behavior, triggers, email content, project configuration, SMTP security modes, GitHub Actions setup, Kubernetes setup, state and deduplication, migration from Slack, troubleshooting, and implementation reference. | ✅ | 2026-07-17 |
| TASK-034 | In `README.md`, replace the Slack secret example with `EMAIL_SMTP_PASSWORD` and link the notifications guide from the feature or operations index. | ✅ | 2026-07-17 |
| TASK-035 | In `docs/onboarding-a-new-project.md`, replace Slack setup with an email setup step that adds the `notifications.email` block before setting the SMTP password secret. | ✅ | 2026-07-17 |
| TASK-036 | In `docs/kubernetes.md`, replace `SLACK_WEBHOOK_URL` examples with a Secret-backed `EMAIL_SMTP_PASSWORD` `fetcher.extraEnv` example and link `docs/notifications.md`. | ✅ | 2026-07-17 |
| TASK-037 | In `docs/github-issues.md`, replace the stale `Slack/Teams notifications` wording with `email notifications` and link the new guide. | ✅ | 2026-07-17 |
| TASK-038 | In `AGENTS.md`, change the package map entry from `Slack notifications` to `email notifications`, update setup examples, and remove any remaining Slack environment references. Do not alter unrelated stale documentation in this feature. | ✅ | 2026-07-17 |
| TASK-039 | Add `[Unreleased]` changelog entries under `Removed` and `Added`: remove `SLACK_WEBHOOK_URL` and Slack webhook delivery; add SMTP email configuration and `EMAIL_SMTP_PASSWORD`. State that consumers must migrate before upgrading if they require notifications. | ✅ | 2026-07-17 |
| TASK-040 | Run `rg -n -i 'slack|SLACK_WEBHOOK_URL|incoming webhook|Block Kit'` across production code, workflows, configs, and active docs. The only permitted matches are immutable historical changelog entries that explicitly describe old releases. | ✅ | 2026-07-17 |

### Implementation Phase 5

- GOAL-005: Replace Slack-focused tests with deterministic email and SMTP coverage.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-041 | Rewrite `backend/internal/notify/notify_test.go` to use a fake `Sender` that records `Message` values and can fail selected sends. Preserve coverage for new failures, changed hashes, duplicate suppression, recovery, below-threshold failures, JobID-based deduplication, newest AI lookup, and state round-trip. | ✅ | 2026-07-17 |
| TASK-042 | Add tests proving failed new and changed alerts are retried because state is not advanced, and failed recoveries are retried because state is not deleted. | ✅ | 2026-07-17 |
| TASK-043 | Add tests proving an old Slack-era state file without `channel` resets and a current `email-v1` state file round-trips. | ✅ | 2026-07-17 |
| TASK-044 | Add `backend/internal/notify/email_test.go` to assert subjects, plain-text labels, HTML escaping, failure and AI truncation, dashboard and Prow links, recovery wording, non-ASCII subject encoding, and CR/LF header sanitization. | ✅ | 2026-07-17 |
| TASK-045 | Add `backend/internal/notify/smtp_test.go` using the injected SMTP client seam. Cover STARTTLS required and used, STARTTLS missing, implicit TLS selection, explicit plaintext selection, authenticated and unauthenticated sends, multiple recipients, sender and recipient envelope commands, MIME structure, context deadline application, and password-free errors. | ✅ | 2026-07-17 |
| TASK-046 | Add project config golden coverage proving the full reference file loads after the email block is added. | ✅ | 2026-07-17 |
| TASK-047 | Run `gofmt -w` on changed Go files and verify `gofmt -l` reports no changed file. Do not reformat unrelated files. | ✅ | 2026-07-17 |
| TASK-048 | Run `cd backend && go test ./internal/notify ./internal/project ./internal/onboard ./internal/fetcher ./internal/e2e -count=1`. | ✅ | 2026-07-17 |
| TASK-049 | Run `make build`, `make build-server`, `make build-worker`, `make test`, `cd backend && go vet ./...`, and `cd backend && staticcheck ./...`. | ✅ | 2026-07-17 |
| TASK-050 | Run `make fe-check`, `make fe-lint`, and `make fe-build` because workflow and documentation changes affect the full repository release contract even though frontend source is unchanged. | ✅ | 2026-07-17 |
| TASK-051 | Validate every local Markdown link and heading anchor after adding `docs/notifications.md` and updating the documentation index. | ✅ | 2026-07-17 |

## 3. Alternatives

- **ALT-001**: Keep Slack and add email as a second transport. Rejected because the requested change is a replacement, the project avoids compatibility branches, and parallel transports add configuration and testing surface.
- **ALT-002**: Use SendGrid, Amazon SES, Mailgun, or another provider-specific HTTP API. Rejected because it would couple the engine to one vendor and require an external dependency or vendor-specific request contract.
- **ALT-003**: Use a third-party Go email library. Rejected because the required SMTP, TLS, and MIME surface is small enough to implement with the standard library, and the project avoids new dependencies without a strong reason.
- **ALT-004**: Invoke a local `sendmail` binary. Rejected because the default container is distroless and does not provide a mail-transfer agent.
- **ALT-005**: Configure all SMTP fields through environment variables. Rejected because it would require many workflow secrets and variables, obscure consumer-owned configuration, and make validation weaker. Only the password belongs in a secret.
- **ALT-006**: Store the SMTP password in `project.yaml`. Rejected because consumer configuration may be public and is processed into runtime output.
- **ALT-007**: Rename the state file to `email_notification_state.json`. Rejected because a channel marker in the existing state provides an explicit migration and avoids leaving an old operational file behind.
- **ALT-008**: Send one digest per fetch. Rejected for this version because it changes alert timing and dedup semantics. A digest can be planned later using the transport and event model created here.
- **ALT-009**: Support SMTP OAuth, custom CA bundles, or insecure certificate skipping in the first version. Rejected to keep the security and configuration contract small. Providers requiring those mechanisms need a trusted SMTP relay.

## 4. Dependencies

- **DEP-001**: Go standard packages `context`, `crypto/tls`, `errors`, `fmt`, `html/template`, `io`, `mime`, `mime/multipart`, `mime/quotedprintable`, `net`, `net/mail`, `net/smtp`, `strings`, and `time`.
- **DEP-002**: Existing `backend/internal/models` flakiness and AI analysis wire types.
- **DEP-003**: Existing `backend/internal/statefile.WriteJSON` atomic writer.
- **DEP-004**: Existing `backend/internal/textutil.Truncate` helper.
- **DEP-005**: Existing fetcher full-pass side-effect ordering in `pipeline.runSideEffects`.
- **DEP-006**: GitHub Actions reusable workflow secret forwarding.
- **DEP-007**: Helm `fetcher.extraEnv` support for Kubernetes Secret references.

## 5. Files

- **FILE-001**: `backend/internal/project/project.go` adds the email configuration schema, defaults, and validation.
- **FILE-002**: `backend/internal/project/project_test.go` covers email configuration and manifest exclusion.
- **FILE-003**: `backend/internal/notify/notify.go` retains notification policy and state while removing Slack transport code.
- **FILE-004**: `backend/internal/notify/email.go` renders plain-text and HTML messages.
- **FILE-005**: `backend/internal/notify/smtp.go` implements SMTP delivery and TLS modes.
- **FILE-006**: `backend/internal/notify/notify_test.go` tests policy, state, retries, and deduplication with a fake sender.
- **FILE-007**: `backend/internal/notify/email_test.go` tests email rendering and escaping.
- **FILE-008**: `backend/internal/notify/smtp_test.go` tests SMTP protocol behavior through the injected client seam.
- **FILE-009**: `backend/internal/fetcher/fetcher.go` constructs and runs the email notifier.
- **FILE-010**: `backend/internal/e2e/pipeline_test.go` isolates email environment state and covers disabled delivery.
- **FILE-011**: `backend/internal/output/output.go` updates notification-state comments while preserving filtering.
- **FILE-012**: `backend/internal/redact/redact.go` and `redact_test.go` remove Slack-specific examples.
- **FILE-013**: `.github/workflows/reusable-deploy.yml` replaces the Slack secret with the SMTP password secret.
- **FILE-014**: `backend/internal/onboard/templates.go` updates generated config, workflows, and checklists.
- **FILE-015**: `backend/internal/onboard/onboard_test.go` validates generated email setup.
- **FILE-016**: `deploy/helm/prow-ai-dashboard/values.yaml` updates notification secret examples.
- **FILE-017**: `configs/example/project.reference.yaml` documents the complete email block.
- **FILE-018**: `docs/notifications.md` is rewritten as the primary email notification guide.
- **FILE-019**: `README.md`, `docs/onboarding-a-new-project.md`, `docs/kubernetes.md`, and `docs/github-issues.md` replace Slack references and link the guide.
- **FILE-020**: `AGENTS.md` updates the repository map and environment references.
- **FILE-021**: `CHANGELOG.md` records the breaking replacement.

## 6. Testing

- **TEST-001**: Project schema accepts a valid authenticated STARTTLS configuration.
- **TEST-002**: Project schema accepts a valid unauthenticated relay configuration.
- **TEST-003**: Project schema applies TLS-specific default ports.
- **TEST-004**: Project schema rejects missing host, sender, recipients, invalid addresses, invalid TLS mode, and invalid port.
- **TEST-005**: Manifest JSON never contains email configuration or recipient addresses.
- **TEST-006**: A first persistent failure sends exactly one failure email and creates state only after success.
- **TEST-007**: An unchanged error hash sends no duplicate email.
- **TEST-008**: A changed error hash sends one changed-failure email and advances state only after success.
- **TEST-009**: A recovery sends one recovery email and deletes state only after success.
- **TEST-010**: Delivery failures are retried on the next processing pass.
- **TEST-011**: A legacy state file without `channel: email-v1` resets and sends initial email notifications.
- **TEST-012**: Failure email contains all required fields and links.
- **TEST-013**: Recovery email contains the prior failure count and dashboard link.
- **TEST-014**: HTML content escapes untrusted test, log, and AI text.
- **TEST-015**: MIME output contains valid plain-text and HTML alternatives and parses with standard mail readers.
- **TEST-016**: STARTTLS mode refuses plaintext downgrade.
- **TEST-017**: Implicit TLS validates the configured SMTP host and TLS 1.2 minimum.
- **TEST-018**: Plaintext SMTP is used only when explicitly configured.
- **TEST-019**: SMTP authentication is omitted for internal relays and uses PLAIN auth when configured.
- **TEST-020**: Multiple configured recipients receive SMTP envelope `RCPT TO` commands and appear in the `To` header.
- **TEST-021**: SMTP errors contain no password or credential-bearing URL.
- **TEST-022**: The generated reusable workflow and onboard scaffold contain `EMAIL_SMTP_PASSWORD` and no Slack secret.
- **TEST-023**: Full backend build, test, vet, and staticcheck gates pass.
- **TEST-024**: Frontend type-check, lint, and build gates pass.
- **TEST-025**: Documentation link and anchor validation passes.

## 7. Risks & Assumptions

- **RISK-001**: SMTP servers vary in authentication and TLS behavior. The initial contract supports PLAIN auth, STARTTLS, implicit TLS, and unauthenticated relays only.
- **RISK-002**: Migration resets Slack-era notification state and can send a burst of emails for all currently persistent failures. This is intentional so the new channel is not silently empty, and the migration must be called out in the changelog and notifications guide.
- **RISK-003**: One message per event may produce high email volume on a dashboard with many persistent failures. This preserves current behavior; digesting is a separate feature.
- **RISK-004**: Recipient addresses in a public consumer `project.yaml` are publicly visible even though they are excluded from `manifest.json`. The documentation must recommend a team distribution list rather than personal addresses when the consumer repository is public.
- **RISK-005**: `net/smtp` is frozen but remains in the Go standard library. The sender seam keeps a future transport replacement isolated.
- **RISK-006**: SMTP delivery can block if deadlines are not applied to every connection. Tests must verify the deadline path.
- **RISK-007**: HTML emails can become injection vectors if dynamic content is not escaped. Rendering must use `html/template` or explicit escaping only.
- **RISK-008**: Header injection is possible if test names or project names contain CR/LF. Header sanitization and validation are mandatory.
- **ASSUMPTION-001**: Consumers can provide an SMTP relay reachable from GitHub-hosted runners or from their Kubernetes cluster.
- **ASSUMPTION-002**: SMTP PLAIN authentication over TLS is sufficient for the first supported provider set.
- **ASSUMPTION-003**: Sender and recipient addresses are acceptable as consumer-owned non-secret configuration.
- **ASSUMPTION-004**: The existing persistent-failure threshold, error-hash resend rule, and recovery rule remain the desired notification policy.
- **ASSUMPTION-005**: Removing Slack without a compatibility period is acceptable because the engine is under active development with a small known consumer set.

## 8. Related Specifications / Further Reading

- `backend/internal/notify/notify.go`
- `backend/internal/fetcher/fetcher.go`
- `backend/internal/project/project.go`
- `.github/workflows/reusable-deploy.yml`
- `deploy/helm/prow-ai-dashboard/values.yaml`
- `docs/onboarding-a-new-project.md`
- `docs/kubernetes.md`
