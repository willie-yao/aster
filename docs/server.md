# Server mode (Kubernetes-native)

The dashboard ships two coequal deploy paths from one codebase:

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

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /data/*` | The fetcher output tree at read parity: `manifest.json`, `dashboard.json`, `jobs/*.json`, `flakiness.json`, `search-index.json`. |
| `GET /api/capabilities` | Deploy descriptor, for example `{"mode":"server","features":{"chat":false,"actions":false}}`. |
| `GET /healthz` | Liveness and readiness probe. |
| `GET /` | The built SPA, when `-static-dir` is set, with deep-link fallback to `index.html`. |
| `POST /api/failures/{id}/create-issue` | Admin-gated: file a GitHub issue for one failure. Enabled only when actions are configured. |
| `POST /api/failures/{id}/propose-fix` | Admin-gated: draft a fix PR for one failure. |

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
auth mechanism. When enabled, the server sets `features.actions: true` and the
frontend shows the buttons.

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
check, and tokens are never logged or returned to the browser.

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

   ```bash
   make fe-build
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
| `ADMIN_LOGINS` | Optional; restrict which header identities may act. |

## Running locally

```bash
# Fetch data first (see docs/development.md), then serve it:
make serve                 # builds bin/server, serves frontend/public/data

# Or serve a self-contained build (SPA + data from one origin):
make fe-build
./bin/server -data-dir=frontend/public/data -static-dir=frontend/dist
```

Flags: `-addr` (default `:8080`), `-data-dir` (default `data`), `-static-dir`
(optional built SPA; empty serves data and API only). Add `-project-dir` plus
the `AUTH_MODE` env above to enable admin actions.
