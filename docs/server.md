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

The server is independent of how the data was analyzed. The in-process worker
and the strategic [Orka preview](../experimental/orka/) pipeline produce the same
`jobs/*.json`, so the server serves both identically. See
[kubernetes.md](kubernetes.md#analysis-backend-in-process-or-orka) for the
backend choice.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /data/*` | The fetcher output tree at read parity: `manifest.json`, `dashboard.json`, `jobs/*.json`, `flakiness.json`, `search-index.json`. |
| `GET /api/capabilities` | Deploy descriptor, for example `{"mode":"server","features":{"actions":false}}`. |
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
   | `OAUTH_SCOPE` | Optional; defaults to `repo`. Use `public_repo` for public-only. |
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
