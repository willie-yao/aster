# Server mode (Kubernetes-native)

The dashboard supports two deployment environments from one codebase:

- **Kubernetes-native (this page).** A small Go server (`backend/cmd/server`)
  serves the dashboard and its JSON over HTTP alongside the inference stack,
  reading from a shared volume a worker or CronJob writes. It adds a capability
  descriptor and admin-gated interactive actions on top of the read contract.
- **Static.** The fetcher writes JSON, GitHub Actions builds the SPA, and GitHub
  Pages serves it. Public, cheap, no backend.

Server mode is a strict superset of the static contract: it serves the exact
same `/data/*.json` files the SPA already reads, then adds a capability
descriptor the frontend uses to discover server-only features. The static path
keeps working unchanged, and all `/data/*.json` schemas stay byte-compatible.

The server is independent of where the in-process analyzer ran. Pages and
Kubernetes deployments produce the same `jobs/*.json`, so the server contract
remains identical.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /data/*` | The fetcher output tree at read parity: `manifest.json`, `dashboard.json`, `jobs/*.json`, `flakiness.json`, `search-index.json`. |
| `GET /api/capabilities` | Deploy descriptor, for example `{"mode":"server","features":{"actions":false}}`. |
| `GET /api/analysis-traces` | Admin-gated private trace snapshot. Exact filters: `job_id`, `build_id`, `test_name`, `outcome`, and `response_id`. |
| `GET /api/analysis-traces/download` | Admin-gated attachment form of the same filtered trace snapshot. |
| `POST /api/analysis-chat/sessions` | Start an owner-bound conversation for one published test analysis. |
| `GET /api/analysis-chat/sessions/{id}` | Read the owning admin's current persisted conversation. |
| `POST /api/analysis-chat/sessions/{id}/messages` | Ask one bounded follow-up question and wait for the final transcript. |
| `POST /api/analysis-chat/sessions/{id}/messages/stream` | Start or reconnect to a turn over SSE progress events. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/cancel` | Cancel one active owner-bound turn. |
| `POST /api/analysis-chat/sessions/{id}/source-investigations` | Run one source investigation for a completed chat request and wait for the result. |
| `POST /api/analysis-chat/sessions/{id}/source-investigations/stream` | Start or reconnect to a source investigation over SSE progress events. |
| `GET /api/analysis-chat/sessions/{id}/source-investigations/{requestID}` | Read the persisted owner-bound investigation state. |
| `POST /api/analysis-chat/sessions/{id}/source-investigations/{requestID}/cancel` | Cancel one active source investigation. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/fix/preview` | Generate an existing fix preview from one selected evidence-backed chat response and optional verified source investigation. |
| `POST /api/analysis-chat/sessions/{id}/requests/{requestID}/correction/preview` | Preview an evidence-backed proposed correction. |
| `POST /api/analysis-corrections/confirm` | Explicitly confirm a preview token and publish the correction overlay. |
| `POST /api/analysis-corrections/{id}/revoke` | Revoke a correction and restore the original analysis. |
| `GET /healthz` | Liveness and readiness probe. |
| `GET /` | The built SPA, when `-static-dir` is set, with deep-link fallback to `index.html`. |
| `POST /api/failures/{id}/create-issue/preview` | Admin-gated: render the exact GitHub issue for one failure without filing it. Enabled only when actions are configured. |
| `POST /api/failures/{id}/propose-fix/preview` | Admin-gated: generate and render the exact draft fix PR for one failure without opening it. |
| `POST /api/actions/confirm` | Admin-gated: file the issue or open the PR previewed under the posted `{"token":...}`. |
| `POST /api/failures/{id}/{action}/requests` | Create a persisted asynchronous issue or fix draft request. Pass `supersedes_request_id` to atomically replace an active request. |
| `GET /api/action-requests/{id}` | Read the owning admin's pending, ready, failed, or confirmed request. |
| `POST /api/action-requests/{id}/confirm` | Post the exact persisted ready draft. |
| `POST /api/action-requests/{id}/cancel` | Cancel a pending or ready request. |
| `POST /api/failures/{id}/resolve` | Admin-gated: mark a recurring pattern resolved at its latest-build watermark. |
| `POST /api/failures/{id}/unresolve` | Admin-gated: remove the resolved marker. |

## Capability seam

The frontend discovers its mode by probing `/api/capabilities`:

- In static Pages mode the endpoint does not exist, the probe fails, and the
  frontend stays in read-only static mode.
- In server mode the descriptor is present, and the frontend lights up only the
  features it advertises.

Interactive features are additive and gated behind this descriptor, so the same
build serves both targets. All `/data/*.json` schemas stay byte-compatible.

## Analysis chat API

The server can expose an authenticated, read-only conversation API for a single
published test analysis. Set `ANALYSIS_CHAT_ENABLED=1` with `-project-dir`,
`AUTH_MODE`, `AI_TOKEN`, and the normal AI provider configuration. Chat mode
disables GitHub actions by default and does not require `BOT_TOKEN`. Set
`ACTIONS_ENABLED=1` only when the same server should expose write actions. The server
then advertises `features.analysis_chat: true`. Static Pages deployments do not
advertise or serve the API. In the Helm chart, set `server.chat.enabled=true`
alongside `ai.enabled=true`; authentication uses the settings under
`server.actions`, but write actions remain disabled.

Create a session by posting the selected analysis identity with a unique
`Idempotency-Key` header:

```json
{
  "job_id": "periodic-demo",
  "build_id": "123",
  "test_name": "TestCluster",
  "suite_name": "cluster lifecycle",
  "class_name": "e2e",
  "junit_file": "junit_01.xml",
  "analysis_generated_at": "2026-07-23T12:00:00Z"
}
```

`suite_name`, `class_name`, and `junit_file` disambiguate duplicate test
names. `analysis_generated_at` is
optional, but including it prevents a conversation from silently attaching to a
newer analysis after the page was loaded. A mismatch returns `409 Conflict`.

Post `{"message":"What evidence supports this?"}` to the session's `messages`
endpoint with a new `Idempotency-Key` for that question. Retrying either POST
with the same key and the same body returns the original session state without
creating another session or model turn. Reusing a key for different input
returns `409 Conflict`.

The JSON endpoint waits for the final transcript. The streaming endpoint emits
`progress` events with `queued`, `investigating`, `reading_evidence`,
`evaluating`, `finalizing`, or `cancelling`, followed by a `session` event. It
streams validated phases rather than unreviewed model tokens. Reconnecting with
the same idempotency key follows the already-running turn on any replica.

The response contains the full transcript. User messages include the accepted
request ID so the frontend can reconcile a response lost after the server
committed it. Assistant messages include
an `assessment` of `explains`, `supports`, `challenges`, or `inconclusive`, plus
verified artifact paths and an optional proposed revision. A proposed revision
does not alter `jobs/*.json` or the published analysis.

Sessions are persisted under `ANALYSIS_CHAT_STATE_DIR`, bound to the
authenticated login, limited to ten admitted attempts including failed turns,
and expire after two hours of inactivity by default. A completed or failed turn
refreshes the expiry. The state file is private and excluded
from `/data/*`. Replicas coordinate short state transitions with an advisory
lock on the shared filesystem, while model calls run without holding that lock.
The shared RWX volume must support advisory file locking, atomic rename, and file and directory synchronization.
The persisted file contains private transcripts and selected failure context, so
volume access and backups must be treated as operator-private data.

Application-generated terminal errors carry a private outcome header so the
frontend can distinguish them from an ingress-generated `502` or `504` after a
committed response. An in-flight turn carries a lease longer than the HTTP model
timeout. If a pod
dies before recording the result, another replica marks that request outcome
unknown instead of running the same idempotency key twice. The client reloads
the authoritative session before allowing an explicit retry. Startup cleanup,
a lifecycle-bound periodic cleanup loop, and request-time cleanup remove
expired sessions from the persisted file and release global and per-owner
capacity.

Operational settings:

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `ANALYSIS_CHAT_STATE_DIR` | `<data-dir>/.analysis-chat` | Private persisted session state and lock file. A path beneath the public data root must use a dot-prefixed top-level directory. |
| `ANALYSIS_CHAT_SESSION_TTL` | `2h` | Conversation retention. |
| `ANALYSIS_CHAT_MAX_SESSIONS` | `128` | Deployment-wide live-session cap. |
| `ANALYSIS_CHAT_MAX_SESSIONS_PER_OWNER` | `8` | Per-login live-session cap. |
| `ANALYSIS_CHAT_MAX_ACTIVE_TURNS_PER_OWNER` | `2` | Concurrent background turns per login. |
| `ANALYSIS_CHAT_REQUESTS_PER_MINUTE` | `10` | Newly admitted turns per login in a rolling minute. |
| `ANALYSIS_CHAT_TIMEOUT` | `2m` | Background turn bound; may only be shortened. |

The agent uses only the configured read-only filesystem and Kubernetes artifact
tools. It has no shell, repository write, or GitHub action capability.
Cancellation is idempotent for completed requests. A disconnected streaming
client does not cancel the server-owned turn; it can reconnect with the same
request ID. Server shutdown and explicit cancellation stop the background model
context and persist a terminal cancelled outcome.

## Source investigation API

Source investigation is an optional Kubernetes-native extension to analysis
chat. Set `ANALYSIS_SOURCE_INVESTIGATION_ENABLED=1` and configure
`ai.source_investigation` in `project.yaml`. The server then advertises
`features.source_investigation: true`. Static Pages mode never serves or
advertises this capability. The dashboard adds an **Investigate source** control
to completed assistant responses and follows persisted progress over SSE. Users
can reconnect to an interrupted stream, cancel an active Task, and review the
verified finding and citations in the conversation.

A source request starts from one completed chat request, not an arbitrary prompt.
Post the chat request ID with a new `Idempotency-Key`:

```json
{"chat_request_id":"chat-request-123"}
```

The server binds the request to the authenticated session owner and snapshots the
selected build, published analysis generation timestamp, chat question, and chat
answer. It resolves `branding.source_repo` only from that build's exact
`repo_refs` entry. The revision must be a full commit SHA. The server never falls
back to the decorated build commit, a branch name, or current `main`. It accepts a
bare full SHA or Prow's unambiguous `ref:fullSHA` form. Composite presubmit refs
are rejected because they do not identify the exact merged checkout. For sessions
created before repository refs were persisted, the server re-reads the same job
and build while requiring the original analysis timestamp to still match.

The runtime creates an Orka agent Task at the pinned revision with Orka's enforced
`orka.ai/agent-read-only` guard. Unsupported guarded runtimes fail before agent
execution. The Task permits repository read tools, disables Bash and edit tools,
and uses a workspace initializer so the read-only Git credential is not mounted
into the agent container. A dedicated server ServiceAccount receives Task-only
create, get, patch, and delete permissions. The runtime rejects any result that
contains a workspace diff or push branch. It never receives `BOT_TOKEN` or
`FIX_TOKEN`.

The agent returns a bounded finding, confidence, relationship to the published
analysis, investigation direction, and source citations. Every citation path and
line range is validated and its quote is checked against the same pinned GitHub
revision before `verified: true` is persisted. Public repositories need no extra
credential. Private repositories require a read-only token in
`SOURCE_INVESTIGATION_GITHUB_TOKEN`; the Helm chart can reuse the AI Secret's
`GITHUB_READ_TOKEN` key. A missing file, changed quote, unsafe path, mutable
revision, or unavailable verification source fails the request instead of
presenting an unverified citation.

Requests share the private analysis chat state file, owner binding, rolling rate
limit, advisory lock, expiry, and replica-safe lease behavior. They continue
after an SSE disconnect and expose only `queued`, `cloning_source`,
`investigating_source`, `verifying_citations`, `finalizing`, or `cancelling`
progress. Cancellation is idempotent and deletes the active Task on timeout or
client cancellation.

Additional settings:

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `ANALYSIS_SOURCE_INVESTIGATION_ENABLED` | `false` | Advertise and serve source investigation. Requires analysis chat. |
| `ANALYSIS_SOURCE_INVESTIGATION_MAX_PER_SESSION` | `8` | Persisted source requests per chat session. |
| `ANALYSIS_SOURCE_INVESTIGATION_MAX_ACTIVE_PER_OWNER` | `1` | Concurrent source Tasks per login. |
| `SOURCE_INVESTIGATION_GITHUB_TOKEN` | empty | Optional read-only token for pinned citation verification in private GitHub repositories. |

## Analysis correction overlays

Set `ANALYSIS_CORRECTIONS_ENABLED=1` together with analysis chat to allow
administrators to promote a structured `challenges` response. Only the agent's
validated `proposed_revision` and verified citations are eligible. The server
persists a private 15-minute preview, requires an explicit confirmation request,
and writes active or revoked corrections to `analysis_corrections.json`.

Corrections never modify `jobs/*.json`. The frontend applies an active correction
only while the exact job, build, test identity, and `analysis_generated_at`
still match. A newer generated analysis makes the overlay visibly stale and
restores the generated conclusion. Revocation also restores the original while
the private ledger retains proposer, confirmer, revoker, session, request, and
audit timestamps.

## Private analysis traces

When admin authentication is configured, the server advertises
`features.analysis_traces: true` and adds a **Traces** page. The page shows the
bounded, content-free metadata from `ai_traces.json`, including response IDs,
provider API mode, request duration and usage, tool names, compaction, critique,
and finalization decisions. Each trace links back to the matching test and build.

The API decodes the known trace schema rather than serving the file directly.
Requests are capped at 64 MiB, responses use `Cache-Control: no-store`, and both
endpoints require the same admin identity used by actions. A missing trace file
returns 404 and the page renders an empty state. Static Pages deployments never
advertise the feature and continue stripping `ai_traces.json` before publication.

## Chat-to-fix bridge

When analysis chat and write actions are both configured, the server advertises
`features.chat_fix: true`. A client can request a fix preview for one successful
assistant response:

```http
POST /api/analysis-chat/sessions/{sessionID}/requests/{chatRequestID}/fix/preview
Content-Type: application/json

{
  "pattern_id": "<recurring-pattern-id>",
  "source_request_id": "<optional-successful-source-request-id>",
  "instruction": "<optional-maintainer-direction>"
}
```

The client selects only identifiers and the optional maintainer instruction. It
cannot submit answer text, revisions, citations, or source findings. The server
reconstructs those fields from the owner-bound private chat state and requires:

- a successful assistant response with verified artifact citations,
- the original published analysis generation and content to remain current,
- analysis freshness and the recurring pattern to come from one job-detail snapshot,
- the recurring pattern to belong to the same job and include the selected build,
- an optional source request to belong to that response and have a successful,
  independently verified result.

Generation receives the selected assistant answer, optional evidence-backed
revision, verified artifact citations, optional verified source finding and
citations, the existing `PatternAnalysis`, and the bounded maintainer
instruction. It never receives the complete transcript. The response is the
normal fix `PreviewResult`; post its token to `/api/actions/confirm` to open the
exact reviewed draft through the existing confirmation workflow. Confirmation state is stored in the shared private volume and remains idempotent
across replicas and restarts for the preview retention window: retrying the same
token after a lost success response returns the original URL, while a concurrent
confirmation returns 409 until the first attempt finishes. The persisted lease
tracks the configured action timeout and carries a fenced attempt ID, so a stale
completion cannot overwrite a newer retry.

The dashboard exposes **Use this finding in a fix proposal** only for completed
evidence-backed responses whose selected build belongs to an actionable recurring
pattern. Before generation, the user reviews the selected pattern, assistant
answer, proposed revision, artifact citations, optional successful source result,
and maintainer instruction. The generated draft then uses the existing preview and
confirmation UI.

## Admin-gated actions

The write endpoints let an admin file an issue or draft a fix PR for a specific
failure on demand, reusing the same engines the scheduled fetch uses. They are
off unless the server is started with `-project-dir` and `AUTH_MODE` selects an
auth mechanism. When enabled, the server sets `features.actions: true` for
resolve controls. It also sets `features.action_requests: true` when the action
runner supports persistent drafts, which enables the issue and fix controls.

File issue and Mark resolved work in the standard server image. Propose fix
starts the local `opencode` runtime and also needs git. The standard distroless
image contains neither tool, so fix previews report unavailable unless you
deploy a custom server image that includes them.

Systemic-pattern email links can deep-link into this flow with the public pattern
id and requested action. The link itself is an inert GET. After authentication,
the frontend requires an explicit **Generate draft** click before creating a
persistent action request.

The frontend review flow posts to `/api/failures/{id}/{action}/requests`, polls
the returned request, and shows the exact issue or draft fix PR before anything
is posted to GitHub. Refining a ready draft creates a replacement request and
atomically cancels the superseded request. The old request exposes
`superseded_by` so clients can recover the replacement after a lost response.
Confirmation posts the persisted
draft through `/api/action-requests/{id}/confirm`. Requests are bound to the
admin who generated them and expire after 24 hours.

The synchronous `*/preview` and `/api/actions/confirm` endpoints expose the same
two-phase contract for direct API clients. Their preview token is single-use,
expires after 15 minutes, and is bound to the admin who generated it.

Two auth modes, both keeping the admin allowlist (`ADMIN_LOGINS`):

- **`oauth`** (per-user attribution): the operator registers a GitHub OAuth App.
  Admins sign in with GitHub; the server holds each admin's own OAuth token in
  an encrypted, httpOnly session cookie and performs the write as them, so the
  issue or PR is attributed to the real user. No token is ever entered in the
  browser. Needs `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`, `OAUTH_REDIRECT_URL`
  (the App's callback), and `SESSION_KEY`.
- **`proxy`** (bot attribution): an upstream SSO proxy (oauth2-proxy, IAP, ...)
  authenticates the user and passes their identity in a trusted header
  (`AUTH_PROXY_HEADER`, e.g. `X-Auth-Request-Email`); a single `BOT_TOKEN`
  performs the write. Simplest when you already run an authenticating proxy.

The `Authenticator` is a seam, so the two modes share one code path and a third
mechanism can be added without touching the handlers. Sessions are stateless
(encrypted cookie), CSRF is covered by a `SameSite=Lax` cookie plus an Origin
check, and tokens are never logged or returned to the browser. Behind a reverse
proxy that serves a public hostname but forwards a different `Host` to the
server (e.g. Azure Front Door), the Origin check needs the public origin: in
oauth mode the `OAUTH_REDIRECT_URL` host is trusted automatically, and
`TRUSTED_ORIGINS` adds any others (required in proxy mode).

### Auth endpoints (oauth mode)

| Path | Purpose |
| --- | --- |
| `GET /api/auth/login` | Redirect to GitHub to sign in. |
| `GET /api/auth/callback` | OAuth callback; establishes the session. |
| `GET /api/auth/user` | The signed-in admin, or 401. |
| `POST /api/auth/logout` | Clear the session. |

### Setting up oauth mode

1. Register a GitHub OAuth App at
   <https://github.com/settings/developers> -> **New OAuth App** (or under an
   org: **Settings -> Developer settings -> OAuth Apps**). Set:
   - **Application name**: anything, e.g. `myproject-dashboard`.
   - **Homepage URL**: your dashboard URL, e.g. `https://dashboard.example.com`
     (or `http://localhost:8080` for local testing).
   - **Authorization callback URL**: the dashboard URL plus
     `/api/auth/callback`, e.g. `https://dashboard.example.com/api/auth/callback`
     (or `http://localhost:8080/api/auth/callback` locally). This must match
     `OAUTH_REDIRECT_URL` exactly.
2. Click **Register application**, then **Generate a new client secret**. Copy
   the client ID and secret.
3. Generate a session key (any long random string), e.g.
   `openssl rand -base64 32`.
4. Run the server with these env vars:

   | Variable | Purpose |
   | --- | --- |
   | `AUTH_MODE=oauth` | Select OAuth login. |
   | `OAUTH_CLIENT_ID` | The App's client ID. |
   | `OAUTH_CLIENT_SECRET` | The App's client secret. |
   | `OAUTH_REDIRECT_URL` | The callback URL registered above. |
   | `SESSION_KEY` | Random secret seeding the session-cookie encryption. |
   | `ADMIN_LOGINS` | Comma-separated GitHub logins allowed to act. |
   | `OAUTH_SCOPE` | Optional; defaults to `repo` with actions and `read:user` for chat-only. |
   | `COOKIE_INSECURE=1` | Optional; allow the cookie over plain http for local testing only. |
   | `TRUSTED_ORIGINS` | Optional; extra public origins the CSRF guard accepts (comma-separated) when behind a proxy. The `OAUTH_REDIRECT_URL` host is trusted automatically. |

   ```bash
   make build-server fe-build
   AUTH_MODE=oauth COOKIE_INSECURE=1 \
   OAUTH_CLIENT_ID=<client-id> OAUTH_CLIENT_SECRET=<client-secret> \
   OAUTH_REDIRECT_URL=http://localhost:8080/api/auth/callback \
   SESSION_KEY="$(openssl rand -base64 32)" ADMIN_LOGINS=your-login \
   ./bin/server -data-dir=frontend/public/data -static-dir=frontend/dist \
     -project-dir=../myproject-dashboard
   ```

   Open <http://localhost:8080>, go to a failing job's pattern, click **Sign in
   to file issues or fixes**, authorize, and the action buttons appear.

### Setting up proxy mode

Use this when an authenticating proxy (oauth2-proxy, Google IAP, ...) already
sits in front of the server and injects the signed-in user in a header. The
server trusts that header, so it must be reachable **only** through the proxy.

| Variable | Purpose |
| --- | --- |
| `AUTH_MODE=proxy` | Select proxy mode. |
| `AUTH_PROXY_HEADER` | Header carrying the user, e.g. `X-Auth-Request-Email`. |
| `BOT_TOKEN` | GitHub PAT that performs the writes (bot account). |
| `ADMIN_LOGINS` | Required comma-separated allowlist of identities that may act. An empty list fails closed. |
| `TRUSTED_ORIGINS` | Public origin(s) the CSRF guard accepts (comma-separated), e.g. `https://dash.example.net`. Required when the proxy's public host differs from the forwarded `Host`. |

## Running locally

```bash
# Fetch data first (see docs/development.md), then serve it:
make serve                 # builds bin/server, serves frontend/public/data

# Or serve a self-contained build (SPA + data from one origin):
make build-server fe-build
./bin/server -data-dir=frontend/public/data -static-dir=frontend/dist
```

Flags: `-addr` (default `:8080`), `-data-dir` (default `data`), `-static-dir`
(optional built SPA; empty serves data and API only). Add `-project-dir` plus
the `AUTH_MODE` env above to enable admin actions.


## Asynchronous action requests

Email deep links and the in-page issue and fix buttons use persistent action
requests. Generation runs in the server process while request metadata and ready
drafts are stored in `action_request_state.json`. The state file is not served
under `/data`.
Requests expire after 24 hours, are bound to the requesting authenticated login,
and require the current user's GitHub token only when generating or confirming.
Raw GitHub tokens are never persisted. A server restart marks unfinished pending
requests failed; ready drafts survive and remain reviewable.

When `notifications.email.action_links` is enabled and the server receives
`EMAIL_SMTP_PASSWORD`, it emails the configured recipients after a draft becomes
ready. The review link still requires the same authenticated login that created
the request.
