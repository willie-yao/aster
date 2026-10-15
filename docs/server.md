# Server mode

Kubernetes server mode serves the same public `/data/*.json` contract as GitHub Pages and adds authenticated APIs. The frontend probes `/api/capabilities` and enables only the features the server advertises. A static Pages deployment has no capability endpoint and remains read-only.

The server reads data written by the authoritative in-process fetcher or worker. It does not own scheduled failure analysis and does not change the public data schema.

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
| `GET /api/analysis-chat/sessions` | List retained operator conversations, filtered by `job_id` and `scope`, with `cursor` pagination and `limit` from 1 to 100 (default 20). |
| `POST /api/analysis-chat/sessions/{id}/archive` | Archive an idle conversation without deleting its transcript. |
| `POST /api/analysis-chat/sessions` | Start or restore the shared chat for one current published analysis. |
| `POST /api/analysis-chat/sessions/lookup` | Restore the shared non-expired conversation for one current analysis. |
| `POST /api/analysis-chat/prepared/lookup` | Read-only batch check of which causes already have a prepared finding waiting, so a collapsed cause control can say so without opening a shared session. |
| `GET /api/analysis-chat/sessions/{id}` | Read a shared conversation and its current active turn. |
| `DELETE /api/analysis-chat/sessions/{id}` | Remove an idle shared conversation. This does not cancel an admitted Fix proposal. |
| `POST /api/analysis-chat/sessions/{id}/messages` | Run one bounded follow-up and return the final transcript. |
| `POST /api/analysis-chat/sessions/{id}/messages/stream` | Start or reconnect to a turn through SSE progress. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/cancel` | Cancel one active turn. Only the operator who started it may cancel it. |
| `POST /api/pull-requests/{number}/checks/{jobID}/builds/{buildID}/escalation` | Start one on-demand analysis of a pull request failure the deterministic pass could not explain. |
| `GET /api/pull-requests/{number}/checks/{jobID}/builds/{buildID}/escalation` | Read that escalation's current state. |
| `POST /api/shared-failures/{id}/escalation` | Start one on-demand analysis of a failure shared across several open pull requests. |
| `GET /api/shared-failures/{id}/escalation` | Read that escalation's current state. |
| `GET /api/failures/{id}/eligibility` | Run deterministic action eligibility and pinned-source preflight without generating a draft. |
| `POST /api/failures/{id}/create-issue/preview` | Preview an issue without filing it. |
| `POST /api/failures/{id}/propose-fix/preview` | Preview a Fix PR without opening it. Registered only when Fix is enabled. |
| `GET /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/branches` | Report the tested branch and the repository's default branch for an exact-analysis Fix. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/requests` | Admit a test- or cause-scoped chat finding, anchored to one exact failed JUnit test or build-level failure, for asynchronous Fix preview generation. `branch_target` is `tested` (the default) or `default`. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/preview` | Synchronous pattern chat-to-fix preview for direct API clients. |
| `POST /api/failures/{id}/{action}/requests` | Create a persistent asynchronous issue or Fix draft request. |
| `GET /api/action-requests/{id}` | Read the owner's pending, ready, failed, cancelled, or confirmed request. |
| `POST /api/action-requests/{id}/confirm` | Confirm the exact persisted ready draft. |
| `POST /api/action-requests/{id}/cancel` | Cancel a pending or ready request. |
| `POST /api/actions/confirm` | Confirm a short-lived synchronous preview token. |
| `POST /api/failures/{id}/resolve` | Mark an eligible recurring pattern resolved at its current watermark. |
| `POST /api/failures/{id}/unresolve` | Remove the resolved marker. Requires only an existing marker, so a resolution never strands. |
| `POST /api/causes/{signature}/resolve` | Mark one cause of a recurring pattern resolved at that cause's watermark, leaving its siblings active. |
| `POST /api/causes/{signature}/unresolve` | Remove a cause's resolved marker. Requires only an existing marker. |
| `GET /api/auth/login` | OAuth mode: start GitHub sign-in. |
| `GET /api/auth/callback` | OAuth mode: establish the encrypted session. |
| `GET /api/auth/user` | Return the signed-in admin or `401`. |
| `POST /api/auth/logout` | Clear the session. |
| `GET /healthz` | Liveness and readiness. |
| `GET /` | Serve the built SPA with deep-link fallback when `-static-dir` is set. |

See [Pull request triage](pull-request-triage.md#optional-ai-escalation) for escalation eligibility, privacy, and deployment requirements.

## Capability contract

`GET /api/capabilities` is the only frontend feature-discovery seam. It exposes safe engine identity and additive flags for authentication, chat, action requests, issue actions, Fix actions, traces, usage, and related server features. The UI must not infer write capability from a visible button or from `/data/*` content.

Interactive features are independently gated. Analysis chat can be enabled without GitHub writes. File Issue can be enabled without Fix. A server may expose read-only trace and status views while every action flag remains false.

The server sends same-origin security headers, denies framing and MIME sniffing, limits referrer and device capabilities, and keeps scripts and connections on the dashboard origin. MUI runtime styles require inline styles; scripts remain strictly external.

## Authentication and origin protection

Two authentication modes share the same admin allowlist:

- `oauth`: a GitHub OAuth App identifies the user with `read:user`. The encrypted httpOnly session cookie stores the login, not the OAuth access token.
- `proxy`: a trusted upstream SSO proxy authenticates the user and passes the identity in `AUTH_PROXY_HEADER`.

`ADMIN_LOGINS` controls who may use authenticated features. GitHub writes use a separate server-held `BOT_TOKEN`; the signed-in user's identity is never reused as the write credential. Audit records keep the initiating admin login, while GitHub records the bot or contributor identity that performed the write.

Core settings:

| Variable | Purpose |
| --- | --- |
| `AUTH_MODE` | `oauth` or `proxy`. |
| `ADMIN_LOGINS` | Comma-separated allowed identities. |
| `SESSION_KEY` | Random key for encrypted session cookies. |
| `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`, `OAUTH_REDIRECT_URL` | GitHub OAuth App coordinates. |
| `AUTH_PROXY_HEADER` | Trusted identity header in proxy mode. |
| `BOT_TOKEN` | Optional server-held GitHub write credential. It is not required for read-only chat. |
| `GITHUB_READ_TOKEN` | GitHub source reads prefer this token, then `BOT_TOKEN`, then `GITHUB_TOKEN`. |
| `SOURCE_INVESTIGATION_GITHUB_TOKEN` | Overrides that read-token preference for action source checks and verification. |
| `TRUSTED_ORIGINS` | Additional exact public origins accepted for state-changing requests. |

Register the OAuth callback as the dashboard origin plus `/api/auth/callback`. `OAUTH_REDIRECT_URL` must match it exactly.

State-changing requests are protected by a `SameSite=Lax` session cookie plus an Origin check. When a reverse proxy forwards a different `Host` than the public hostname, configure the public origin. OAuth mode trusts the origin in `OAUTH_REDIRECT_URL`; proxy mode normally needs `TRUSTED_ORIGINS` explicitly. Tokens are never returned to the browser or written to logs.

## Analysis chat

Enable chat only with authentication, the normal provider configuration, and a private state directory. Chat is read-only and does not require `BOT_TOKEN`. Static Pages deployments do not serve it. Each model turn defaults to 10 minutes; operators can set `server.chat.timeout` to a value up to 30 minutes.

Interactive test, cause, and pattern turns use the project's effective `ai.max_iters` (default `15`), capped at `100` tool rounds. Engine-prepared cause findings keep an eight-round cap, or the project's limit when lower. Timeout, tool-call, and byte budgets still apply independently.

One shared session is bound to each exact current analysis identity. Authenticated operators see the same transcript and active-turn progress. Only one turn may run at a time, and only the operator who started it may cancel it. Test, recurring-pattern, and causal-group scopes are supported. A causal-group session is bound to the parent pattern ID and hash plus the cause ID and hash. It exposes the cause's failed member builds, including single-build causes, plus the newest later completed run when one is available. The later run is comparison evidence, not a member of the cause. A passing comparison proves only that the cause did not reproduce in that run; the chat must compare the test and triggering inputs before describing it as resolved. Cause chat does not require the parent pattern to be systemic. The server refuses the session when any member build has left the published window.

Interactive cause sessions also bind the newest fully passing retained run before the first member failure when artifact locations are recorded, even if its dashboard JUnit is truncated. **What changed since the last passing run?** is offered before the first question when the published runs contain a qualifying baseline; once a session exists, its optional `baseline_build_id` is authoritative and appears in conversation and history metadata. Chat compares job inputs, commits, rendered resources, and node OS image and kernel before logs. It locates relevant files in each build with `find_artifacts`, searches their changed fields with `grep_artifact`, and reads files when the search does not explain the difference, pairing both builds' reads in one round unless `single_tool_call` is configured. When a failing node never registered and has no `status.nodeInfo`, chat reads its OS version and kernel from that machine's boot or console log. Comparison citations quote only the differing passages, within the 2000-byte limit; long lines retain a verbatim excerpt containing the cited text. The baseline does not change cause membership, recovery status, or Fix targets; missing baselines are reported explicitly.

The resolved evidence is frozen when a session is created. Both comparison and baseline builds are identified in the model context, and a refreshed lookup does not reuse the old shared session when either selected build changes. An already-open session continues to describe the evidence available when it was created. Engine-prepared first answers keep their member and later-comparison scope and cache identity.

Each start or message uses a unique `Idempotency-Key`. Repeating the same key and body returns the original state; reusing the key for different input fails. Streaming clients may reconnect to an existing server-owned turn. Disconnecting does not cancel it, while explicit cancellation and server shutdown persist a terminal cancelled outcome.

Interactive test, cause, and pattern turns have a tool-call cap of four times their effective iteration limit; engine-prepared cause turns retain a 24-call cap. Chat stops starting new tool rounds when the remaining time cannot cover another recent-duration model request plus finalization headroom, then requests the final answer within the original turn deadline. Completed assistant turns store `stop_reason`, also included in the final `analysis chat response:` log: `answered`, `rounds`, `tool_calls`, `time`, or `bytes`; failed requests can log `error`. This distinguishes an investigation limit from the finalization stage.

The model may use only the configured read-only artifact and pinned-source capabilities. It has no shell, GitHub write, repository write, or live cluster access. Citations are verified against the artifacts the conversation actually read, including reads from earlier turns of the same conversation. The quote a citation carries is attributed by the engine from what those reads returned rather than copied from the model, so a citation naming a passage the tools never returned, or one so generic it names several, cannot be verified. Invalid citations are omitted individually. When verified citations remain, the answer is marked partially verified. An answer whose evidence or response format did not pass validation remains unverified. A maintainer may use a completed nonempty answer to start a Fix investigation even without verified citations; its qualification is retained, and only validated citations are carried as verified artifact evidence. A response that only announces the model's next step is not an answer: the engine asks the model to take that step or conclude, and the turn fails if it does neither.

After each initial or reconciliation AI publication, the fetcher prepares up to three active causal groups that do not already have a current finding, preferring causes on published recurring patterns so the budget reaches the causes a maintainer can open. These best-effort findings use the same cause-scoped member and comparison artifacts and citation validation as interactive chat. They are cached by cause, model, prompt, and AI cache generation. A prepared finding seeds a new cause conversation without using one of the maintainer's admitted turns. Preparation failures never fail or roll back dashboard publication, and the remaining causes are retried on later runs.

A proposed revision or Fix finding is still inert model output. Source compatibility, patch generation, and GitHub writes remain user-triggered. Exact-analysis Fix admission requires the conversation's published analysis to remain current; after admission, generation uses the captured analysis and answer, while failure evidence, cause membership and content, and source/base identity are rechecked. Regenerating analysis text alone does not revoke an admitted Fix. See [Fix PR generation](fix-prs.md#exact-failure-analysis-handoff).

Sessions are stored in private shared state and have bounded admitted turns. After `server.chat.sessionTTL` of inactivity (default `2h`), a session becomes read-only and releases its live-session slot. Operator questions, failed attempts, and explicit Fix investigation requests retain their original conversations for `server.chat.historyRetention` (default `4320h`, or 180 days) after activity. Set `ANALYSIS_CHAT_HISTORY_RETENTION` for deployments without Helm. Reads, polling, and scheduled preparation do not extend retention. Prepared-only conversations keep the short session lifetime.

**New conversation** archives the shared session and starts without its previous discussion. For retained sessions, **Save and start new** keeps the previous conversation read-only until its retention deadline; it does not delete it. A prepared-only conversation instead offers **Archive and start new** and explains that archiving does not add it to history. Archiving a conversation that holds a prepared finding dismisses that finding for every operator for `server.chat.historyRetention`, so later conversations for the cause start empty unless a newer comparison run or cause revision is prepared. **Earlier conversations** in an open investigation and **History** in navigation keep saved findings accessible across analysis refreshes and publication-window changes. History is filtered by job and scope, not automatically matched to a durable cause. Saved conversation pages are read-only and distinguish original evidence builds, comparison builds, and pinned source when available. Their citations describe the original evidence, not newer runs. Explicit deletion remains available through the session API. Retained history does not consume live-session limits; the private store still rejects writes over its size limit without replacing existing history.

The server upgrades version-4 chat state before cleanup, preserving session and request IDs, messages, and evidence. Retention is calculated from recorded operator activity rather than upgrade time. The one-time private `sessions.json.v4.bak` backup is operator-managed and is not pruned by history retention. Restoring it is required to roll back to a version-4 server and does not preserve conversations created after the upgrade. Already deleted conversations cannot be recovered.

Fix requests retain their own bounded evidence snapshot, so deleting or expiring a conversation does not cancel an admitted Fix. Use the separate action cancellation endpoint to cancel a pending or ready proposal. The Helm chart uses a `Recreate` server rollout for interactive features so old and new processes do not overlap during upgrades. Equivalent non-Helm deployments must stop old server processes before starting replacements. User questions and attempts retain the initiating operator for attribution. The state contains transcripts and selected failure context, so the RWX volume and backups are operator-private. Chat-only replicas require advisory locking, atomic rename, and file and directory synchronization.

## Admin-gated actions

Actions are disabled unless authentication and a project directory are configured. The server resolves the current published subject and checks the selected action's requirements. Issue drafting retains deterministic remediation eligibility and source verification. Manual Fix attempts do not require an implementation-ready diagnosis, verified citations, or existing source links: transient severity, low confidence, non-systemic classification, and incomplete remediation are investigative context or warnings. Repository authorization, pinned generation bases, patch integrity, and explicit confirmation remain required. Mixed-cause parents do not create one combined proposal; select a cause or existing failure target.

Actions and pull request/shared-failure escalation require a single server process writing each data directory, including during non-Helm upgrades. Stop the old process before starting its replacement. The Helm chart rejects multiple replicas when either feature is enabled.

The common lifecycle is:

1. The owner explicitly requests a draft.
2. The server creates an owner-bound persistent action request or short-lived synchronous preview.
3. The UI shows the exact issue or draft PR content and all warnings.
4. The owner explicitly confirms that exact draft.
5. The server repeats current-data and policy checks, then performs the write with the configured credential.

Persistent requests survive normal restarts when ready. Unfinished external runtime work is reconciled or marked failed. Requests are idempotent and may be atomically superseded by a replacement draft. Confirmation never regenerates content silently.

File Issue and Mark Resolved use the standard server runtime. Fix generation is separate, experimental, and uses Agent Sandbox. Its source, patch, warning, regeneration, and confirmation contracts are in [Fix PR generation](fix-prs.md).

Successful GitHub confirmations append a private write-audit record containing the initiating and confirming login, action kind, target, result URL, timestamps, and reconciliation status. Credentials are not stored.
If audit or request-state persistence fails, the request reconciles the external result before confirmation is reported as complete.

Email delivery after a draft becomes ready is optional and does not change who may review or confirm it. SMTP configuration belongs in [Notifications](notifications.md).

## Private data boundary

Public `/data/*` serving uses an allowlist and rejects private files and hidden directories. Private state includes:

- analysis chat transcripts, prepared cause findings, and locks;
- action requests, preview state, and write audit;
- analysis traces and pattern diagnostics;
- fetch status and pass history;
- AI usage ledgers;

Authenticated APIs expose only sanitized, purpose-specific views. They never return provider credentials, OAuth tokens, bot tokens, prompts, raw model responses, unrestricted source, raw artifact content, or private filesystem paths.

Usage reports preserve provider-reported versus unavailable token fields and use only operator-configured pricing. Trace and diagnostic views contain bounded control-flow codes, counts, durations, and safe identities.

## Run locally

Fetch or copy dashboard data first, then run:

```bash
make serve
```

For a self-contained local server with development authentication and action capabilities:

```bash
make dev-actions PROJECT_DIR=../<consumer-repo>
```

Development authentication is local-only. Do not expose it as a production identity mechanism.
