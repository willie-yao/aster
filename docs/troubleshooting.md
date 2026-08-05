# Troubleshooting

Start with the fetcher or workflow logs. Most first-deploy failures fall into the
cases below.

| Symptom | Likely cause | Resolution |
| --- | --- | --- |
| `AI is enabled but no provider is configured` | `AI_ENDPOINT` or `AI_MODEL` is missing. | Set both repository variables, commit `ai.endpoint` and `ai.model`, or disable AI. |
| `AI_TOKEN is not set, disabling AI analysis` | No bearer token was supplied. | Set `AI_TOKEN`. Use any non-empty placeholder for an unauthenticated endpoint. |
| Missing or empty `prompts/system.md` | AI was enabled without a project prompt. | Add a non-empty prompt under `<project_dir>/prompts/system.md`. |
| `required experimental API prompt draft was not produced` | `--require-prompt-draft` was set and prompt preparation safely fell back. | Read the safe stage and category, fix the reviewed provider or source access, then retry. |
| Prompt drafting times out on a slow provider | Source retrieval plus phased structured extraction exceeded the onboarding-specific budget. | Retry with `--prompt-timeout 30m` or another reviewed value. The fetcher timeout and project `ai.timeout` do not change onboarding drafting. |
| `AI_TOKEN is required because it authenticates experimental API prompt drafting` | Strict prompt drafting was selected without an environment token. | Export `AI_TOKEN`; do not pass it as a flag or endpoint query parameter. |
| Prompt drafting falls back with a safe warning | Source retrieval, structured extraction, grounding, merging, or final validation failed. | Use the reported stage and action. Add `--prompt-debug` for sanitized stderr metadata without source or provider bodies. |
| Local onboarding refuses existing scaffold files | A planned generated file already exists and update mode was not selected. | Choose another dashboard consumer directory or rerun with `--update-existing` after reviewing every replacement. |
| Onboarding warns about stale deployment files | Files from the unselected Pages or Kubernetes mode exist in the destination. | Review them manually. Onboarding leaves them untouched and never deletes them automatically. |
| `AI endpoint rejected tools` | The endpoint or model does not support OpenAI-style function calling. | Enable the provider's tool-call parser or choose a tool-capable model. |
| Zero jobs in `dashboard.json` | Discovery found no matches, or every discovered job failed while loading build data. | Check fetcher storage and artifact errors first, then validate the discovery selector. |
| Pages workflow cannot find `project.yaml` | `project_dir` does not match the consumer layout. | Use `.` for the repository root or the exact subdirectory in the deploy workflow. |
| Dashboard assets return 404 | `branding.base_path` does not match the Pages repository. | Set it to `/<host-repo>` with no trailing slash. |
| Pages site is not deployed | Pages is not configured to use GitHub Actions. | Enable Pages with `gh api .../pages -X POST -F build_type=workflow`. |
| Private endpoint times out | The GitHub-hosted runner cannot reach the network. | Use Kubernetes-native mode, a self-hosted runner, or `skip-fetch` with committed data. |
| Analysis is generic | The project prompt lacks architecture, artifact layout, or real failure signatures. | Expand `prompts/system.md`. The update applies to new analyses; use an intentional cache rebaseline if existing entries must be replaced. |
| Cached analysis came from the old provider | Existing reusable entries retain their provider provenance after a provider change. | Set a new cache generation for a reversible full rebaseline. |
| `Propose fix` reports unavailable | The process cannot find `opencode` or git. | Use a runner or custom server image containing both tools. |
| Helm rejects `server.actions.oauth.scope` or `chatScope` | The chart now derives a least-privilege OAuth scope. | Remove both legacy keys. Keep `server.actions.oauth.privateRepositories=false` for public targets or set it to `true` only for private action targets. |
| OAuth actions cannot access a private repository | The public-only default requested `public_repo`. | Set `server.actions.oauth.privateRepositories=true`, upgrade, then sign out and authorize the OAuth App again. |

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

## OAuth access after a scope change

Chat-only OAuth always requests `read:user`. OAuth actions request
`public_repo` by default and request `repo` only when
`server.actions.oauth.privateRepositories=true`.

After changing private-repository access in either direction, sign out of the
dashboard and sign in again so the session matches the configured policy. When
reducing from `repo` to `public_repo`, revoke the OAuth App in GitHub before
reauthorizing if GitHub retains the previous broad grant. The chart rejects
legacy `scope`, `chatScope`, and `OAUTH_SCOPE` overrides instead of silently
continuing with broader access.

## No jobs were published

A dashboard that loads with zero jobs has a valid frontend and manifest, but the
latest fetch published no job summaries. This has two common causes: discovery
found no matching jobs, or every discovered job failed while loading build data.

1. Check the fetcher logs first. `Warning: N jobs had fetch errors` and per-job
   errors point to storage connectivity, credentials, bucket routing, or malformed
   build data. Fix those errors before changing a valid discovery selector.
2. Run a one-build check without AI:

   ```bash
   ./bin/fetcher -project-dir=../my-consumer -ai=false -builds=1
   ```

3. Inspect the result:

   ```bash
   python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']))"
   ```

4. If the logs contain no job fetch errors, confirm `testgrid.dashboard` exactly
   matches the jobs' `testgrid-dashboards` annotation.
5. For bucket discovery, remove `discovery.job_filters` temporarily and confirm
   the storage provider, bucket, and gcsweb base.
6. Add `source.include_presubmits: true` only when the expected jobs are
   presubmits rather than periodics.

The `onboard` command validates discovery before generating a scaffold. A later
fetch can still publish zero jobs when artifact loading fails for every match.


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
