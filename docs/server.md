# Server mode

Kubernetes server mode serves the same public `/data/*.json` contract as GitHub
Pages and adds authenticated APIs. The frontend probes `/api/capabilities` and
enables only the features the server advertises. A static Pages deployment has
no capability endpoint and remains read-only.

The server reads data written by the authoritative in-process fetcher or worker.
It does not own scheduled failure analysis and does not change the public data
schema.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /data/*` | Public fetcher output such as `manifest.json`, `dashboard.json`, `jobs/*.json`, `flakiness.json`, and `search-index.json`. Private files and dot-prefixed directories are rejected. |
| `GET /api/capabilities` | Safe deployment identity and feature flags used by the frontend. |
| `GET /api/fetch-status` | Admin-only aggregate fetch progress, freshness, and recent pass summaries. `HEAD` is supported. |
| `GET /api/pattern-diagnostics` | Admin-only sanitized recurring-pattern validation and cooldown diagnostics. |
| `GET /api/analysis-health` | Admin-only sanitized analysis runtime records with exact job, build, test, outcome, and response filters. |
| `GET /api/analysis-health/download` | Attachment form of the same filtered report. |
| `GET /api/ai-usage` | Admin-only private usage report with optional date and feature filters. |
| `GET /api/ai-usage/download` | Attachment form of the same usage report. |
| `POST /api/analysis-chat/sessions` | Start an owner-bound chat for one current published analysis. |
| `POST /api/analysis-chat/sessions/lookup` | Restore the owner's latest matching non-expired conversation. |
| `GET /api/analysis-chat/sessions/{id}` | Read the owner's current conversation. |
| `DELETE /api/analysis-chat/sessions/{id}` | Discard the owner's conversation, cancelling any turn still in flight. |
| `POST /api/analysis-chat/sessions/{id}/messages` | Run one bounded follow-up and return the final transcript. |
| `POST /api/analysis-chat/sessions/{id}/messages/stream` | Start or reconnect to a turn through SSE progress. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/cancel` | Cancel one active owner-bound turn. |
| `POST /api/pull-requests/{number}/checks/{jobID}/builds/{buildID}/escalation` | Start one on-demand analysis of a pull request failure the deterministic pass could not explain. |
| `GET /api/pull-requests/{number}/checks/{jobID}/builds/{buildID}/escalation` | Read that escalation's current state. |
| `POST /api/shared-failures/{id}/escalation` | Start one on-demand analysis of a failure shared across several open pull requests. |
| `GET /api/shared-failures/{id}/escalation` | Read that escalation's current state. |
| `GET /api/failures/{id}/eligibility` | Run deterministic action eligibility and pinned-source preflight without generating a draft. |
| `POST /api/failures/{id}/create-issue/preview` | Preview an issue without filing it. |
| `POST /api/failures/{id}/propose-fix/preview` | Preview a Fix PR without opening it. Registered only when Fix is enabled. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/requests` | Admit a test- or cause-scoped chat finding, anchored to one exact JUnit failure, for asynchronous Fix preview generation. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/preview` | Legacy synchronous pattern chat-to-fix preview. |
| `POST /api/failures/{id}/{action}/requests` | Create a persistent asynchronous issue or Fix draft request. |
| `GET /api/action-requests/{id}` | Read the owner's pending, ready, failed, cancelled, or confirmed request. |
| `POST /api/action-requests/{id}/confirm` | Confirm the exact persisted ready draft. |
| `POST /api/action-requests/{id}/cancel` | Cancel a pending or ready request. |
| `POST /api/actions/confirm` | Confirm a short-lived synchronous preview token. |
| `POST /api/failures/{id}/resolve` | Mark an eligible recurring pattern resolved at its current watermark. |
| `POST /api/failures/{id}/unresolve` | Remove the resolved marker. Requires only an existing marker, so a dismissal never strands. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/correction/preview` | Preview an evidence-backed analysis correction. |
| `POST /api/analysis-corrections/confirm` | Confirm and publish the correction overlay. |
| `POST /api/analysis-corrections/{id}/revoke` | Revoke the overlay and restore the original analysis. |
| `GET /api/auth/login` | OAuth mode: start GitHub sign-in. |
| `GET /api/auth/callback` | OAuth mode: establish the encrypted session. |
| `GET /api/auth/user` | Return the signed-in admin or `401`. |
| `POST /api/auth/logout` | Clear the session. |
| `GET /healthz` | Liveness and readiness. |
| `GET /` | Serve the built SPA with deep-link fallback when `-static-dir` is set. |

## Capability contract

`GET /api/capabilities` is the only frontend feature-discovery seam. It exposes
safe engine identity and additive flags for authentication, chat, action
requests, issue actions, Fix actions, corrections, traces, usage, and related
server features. The UI must not infer write capability from a visible button or
from `/data/*` content.

Interactive features are independently gated. Analysis chat can be enabled
without GitHub writes. File Issue can be enabled without Fix. A server may expose
read-only trace and status views while every action flag remains false.

The server sends same-origin security headers, denies framing and MIME sniffing,
limits referrer and device capabilities, and keeps scripts and connections on the
dashboard origin. MUI runtime styles require inline styles; scripts remain
strictly external.

## Authentication and origin protection

Two authentication modes share the same admin allowlist:

- `oauth`: a GitHub OAuth App identifies the user with `read:user`. The encrypted
  httpOnly session cookie stores the login, not the OAuth access token.
- `proxy`: a trusted upstream SSO proxy authenticates the user and passes the
  identity in `AUTH_PROXY_HEADER`.

`ADMIN_LOGINS` controls who may use authenticated features. GitHub writes use a
separate server-held `BOT_TOKEN`; the signed-in user's identity is never reused
as the write credential. Audit records keep the initiating admin login, while
GitHub records the bot or contributor identity that performed the write.

Core settings:

| Variable | Purpose |
| --- | --- |
| `AUTH_MODE` | `oauth` or `proxy`. |
| `ADMIN_LOGINS` | Comma-separated allowed identities. |
| `SESSION_KEY` | Random key for encrypted session cookies. |
| `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`, `OAUTH_REDIRECT_URL` | GitHub OAuth App coordinates. |
| `AUTH_PROXY_HEADER` | Trusted identity header in proxy mode. |
| `BOT_TOKEN` | Optional server-held GitHub write credential. It is not required for read-only chat. |
| `TRUSTED_ORIGINS` | Additional exact public origins accepted for state-changing requests. |

Register the OAuth callback as the dashboard origin plus
`/api/auth/callback`. `OAUTH_REDIRECT_URL` must match it exactly.

State-changing requests are protected by a `SameSite=Lax` session cookie plus an
Origin check. When a reverse proxy forwards a different `Host` than the public
hostname, configure the public origin. OAuth mode trusts the origin in
`OAUTH_REDIRECT_URL`; proxy mode normally needs `TRUSTED_ORIGINS` explicitly.
Tokens are never returned to the browser or written to logs.

## Analysis chat

Enable chat only with authentication, the normal provider configuration, and a
private state directory. Chat is read-only and does not require `BOT_TOKEN`.
Static Pages deployments do not serve it. Each model turn defaults to 10 minutes;
operators can set `server.chat.timeout` to a value up to 30 minutes.

A session is bound to the signed-in owner and one exact current analysis
identity. Test, recurring-pattern, and causal-group scopes are supported. A
causal-group session is bound to the parent pattern ID and hash plus the cause ID
and hash. It exposes exactly the cause's member builds, including single-build
causes, and does not require the parent pattern to be systemic. The server refuses
the session when any member build has left the published window.

Each start or message uses a unique `Idempotency-Key`. Repeating the
same key and body returns the original state; reusing the key for different
input fails. Streaming clients may reconnect to an existing server-owned turn.
Disconnecting does not cancel it, while explicit cancellation and server
shutdown persist a terminal cancelled outcome.

The model may use only the configured read-only artifact and pinned-source
capabilities. It has no shell, GitHub write, repository write, or live cluster
access. Citations are verified against the artifacts the conversation actually
read, including reads from earlier turns of the same conversation. The quote a
citation carries is attributed by the engine from what those reads returned
rather than copied from the model, so a citation naming a passage the tools never
returned, or one so generic it names several, cannot be verified. Invalid
citations are omitted individually. When verified citations remain, the answer
is marked partially verified and may start a Fix investigation with only the
validated citations carried as artifact evidence. An answer with no verified
citation, or one that does not
follow the response format, remains unverified and cannot start a Fix preview or
a correction. Partially verified answers cannot promote corrections. A response
that only announces the model's next step is
not an answer: the engine asks the model to take that step or conclude, and the
turn fails if it does neither.

A proposed correction or Fix finding is still inert model output. Corrections
require their own preview and confirmation. Exact-JUnit Fix handoff requires the
separate lifecycle in [Fix PR generation](fix-prs.md#exact-junit-analysis-handoff).

Sessions are stored in private owner-bound state, have bounded admitted turns,
and expire after inactivity. The state contains transcripts and selected failure
context, so the RWX volume and backups are operator-private. Replicas require
advisory locking, atomic rename, and file and directory synchronization.

## Admin-gated actions

Actions are disabled unless authentication and a project directory are
configured. The server first resolves the current published subject and runs the
same deterministic eligibility, remediation-policy, and pinned-source checks used
at preview and confirmation.

The common lifecycle is:

1. The owner explicitly requests a draft.
2. The server creates an owner-bound persistent action request or short-lived
   synchronous preview.
3. The UI shows the exact issue or draft PR content and all warnings.
4. The owner explicitly confirms that exact draft.
5. The server repeats current-data and policy checks, then performs the write with
   the configured credential.

Persistent requests survive normal restarts when ready. Unfinished external
runtime work is reconciled or marked failed. Requests are idempotent and may be
atomically superseded by a replacement draft. Confirmation never regenerates
content silently.

File Issue and Mark Resolved use the standard server runtime. Fix generation is
separate, experimental, and uses Agent Sandbox. Its source, patch, warning,
regeneration, and confirmation contracts are in [Fix PR generation](fix-prs.md).

Successful GitHub confirmations append a private write-audit record containing
the initiating and confirming login, action kind, target, result URL, timestamps,
and reconciliation status. Credentials are not stored.

Email delivery after a draft becomes ready is optional and does not change who
may review or confirm it. SMTP configuration belongs in
[Notifications](notifications.md).

## Analysis corrections

A challenged chat response may propose a complete evidence-backed revision.
Preview validates the current published analysis, correction content, and cited
evidence without changing `jobs/*.json`. Confirmation publishes a separate
overlay, and revocation restores the original result. Correction state is
private operational data and does not rewrite the authoritative analysis cache.

## Private data boundary

Public `/data/*` serving uses an allowlist and rejects private files and hidden
directories. Private state includes:

- analysis chat transcripts and locks;
- action requests, preview state, and write audit;
- correction overlays;
- analysis traces and pattern diagnostics;
- fetch status and pass history;
- AI usage ledgers;
- optional Agent Sandbox shadow ledgers.

Authenticated APIs expose only sanitized, purpose-specific views. They never
return provider credentials, OAuth tokens, bot tokens, prompts, raw model
responses, unrestricted source, raw artifact content, or private filesystem
paths.

Usage reports preserve provider-reported versus unavailable token fields and use
only operator-configured pricing. Trace and diagnostic views contain bounded
control-flow codes, counts, durations, and safe identities. The server does not
mount the private Agent Sandbox shadow ledger, and it never enters the public data
path.

## Run locally

Fetch or copy dashboard data first, then run:

```bash
make serve
```

For a self-contained local server with development authentication and action
capabilities:

```bash
make dev-actions PROJECT_DIR=../<consumer-repo>
```

Development authentication is local-only. Do not expose it as a production
identity mechanism.
