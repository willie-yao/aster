# Troubleshooting

Start with the fetcher or workflow logs. Most first-deploy failures fall into the
cases below.

| Symptom | Likely cause | Resolution |
| --- | --- | --- |
| `AI is enabled but no provider is configured` | `AI_ENDPOINT` or `AI_MODEL` is missing. | Set both repository variables, commit `ai.endpoint` and `ai.model`, or disable AI. |
| `AI_TOKEN is not set, disabling AI analysis` | No bearer token was supplied. | Set `AI_TOKEN`. Use any non-empty placeholder for an unauthenticated endpoint. |
| Missing or empty `prompts/system.md` | AI was enabled without a project prompt. | Add a non-empty prompt under `<project_dir>/prompts/system.md`. |
| `AI endpoint rejected tools` | The endpoint or model does not support OpenAI-style function calling. | Enable the provider's tool-call parser or choose a tool-capable model. |
| Zero jobs in `dashboard.json` | The TestGrid dashboard does not match job annotations, or bucket discovery is filtered too narrowly. | Run a one-build discovery sweep and correct `testgrid.dashboard` or `discovery.job_filters`. |
| Pages workflow cannot find `project.yaml` | `project_dir` does not match the consumer layout. | Use `.` for the repository root or the exact subdirectory in both workflows. |
| Dashboard assets return 404 | `branding.base_path` does not match the Pages repository. | Set it to `/<host-repo>` with no trailing slash. |
| Pages site is not deployed | Pages is not configured to use GitHub Actions. | Enable Pages with `gh api .../pages -X POST -F build_type=workflow`. |
| Private endpoint times out | The GitHub-hosted runner cannot reach the network. | Use Kubernetes-native mode, a self-hosted runner, or `skip-fetch` with committed data. |
| Analysis is generic | The project prompt lacks architecture, artifact layout, or real failure signatures. | Expand `prompts/system.md` and let prompt fingerprinting refresh affected entries. |
| Cached analysis came from the old provider | A replacement analysis has not completed yet. | Provider fingerprinting refreshes it automatically. Clear the cache only for an immediate full rebaseline. |
| `Propose fix` reports unavailable | The process cannot find `opencode` or git. | Use a runner or custom server image containing both tools. |
| Orka Tasks remain pending | Orka, the Provider, tool shim, compatibility worker, or RBAC is incomplete. | Follow the experimental Orka quickstart and inspect Task and ai-worker status. |

## Useful checks

```bash
# Validate config and job discovery without AI.
./bin/fetcher -project-dir=../my-consumer -ai=false -builds=1

# Inspect the generated job count.
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']))"

# Pages workflow logs.
gh run list --workflow deploy.yml
gh run view --log-failed

# Kubernetes-native health and capabilities.
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/api/capabilities
```

For deeper AI-loop behavior, see the troubleshooting section in
[Agentic analysis](agentic.md#troubleshooting).


## Email notifications are not sent

Check the fetcher logs for the email notification summary or configuration
warning. Confirm:

- `notifications.email.enabled` is true.
- `from`, at least one `to` recipient, and `smtp.host` are configured.
- `EMAIL_SMTP_PASSWORD` is present when `smtp.username` is set.
- The SMTP relay is reachable from the GitHub Actions runner or Kubernetes pod.
- `smtp.tls` matches the relay. STARTTLS is required by default and never falls
  back to plaintext.

A failed delivery does not fail the fetch. Its state is left unchanged so the
next full pass retries it.


## Email action link does not show issue or fix controls

Email action links require all of the following:

- `notifications.email.action_links: true`.
- A Kubernetes-native server deployment rather than static Pages.
- `server.actions.enabled: true` with OAuth or proxy authentication.
- The signed-in identity is present in `server.actions.admins`.
- The recurring pattern still exists in the current job data.

Opening the link only displays an intent prompt. Click **Generate draft** before
the dashboard calls the preview API. Fix proposals also require `opencode` and
git in the server image.


## Asynchronous draft stays pending or no ready email arrives

- Check the server logs and `GET /api/action-requests/<id>` status.
- A server restart marks an unfinished pending request failed because user tokens
  are intentionally never persisted. Start a new request from the pattern.
- Ready drafts persist for 24 hours in non-public `action_request_state.json`.
- Draft-ready email requires `EMAIL_SMTP_PASSWORD` in `server.extraEnv`, not only
  `fetcher.extraEnv`.
- The review link is bound to the authenticated login that created the request.
