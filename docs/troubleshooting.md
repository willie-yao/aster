# Troubleshooting

Start with the fetcher or workflow logs. Most first-deploy failures fall into the
cases below.

| Symptom | Likely cause | Resolution |
| --- | --- | --- |
| `AI is enabled but no provider is configured` | `AI_ENDPOINT` or `AI_MODEL` is missing. | Set both deployment values or disable AI. |
| `AI_TOKEN is not set, disabling AI analysis` | No bearer token was supplied. | Set `AI_TOKEN`. Use any non-empty placeholder for an unauthenticated endpoint. |
| Missing or empty `prompts/system.md` | AI was enabled without a project prompt. | Add a non-empty prompt under `<project_dir>/prompts/system.md`. |
| Prompt authoring times out | Source revision resolution exceeded the onboarding-specific budget. | Retry with `--prompt-timeout 30m` or use `--prompt-mode=todo-template`. The fetcher timeout and project `ai.timeout` do not change onboarding prompt authoring. |
| Prompt authoring falls back with a safe warning | Source revision resolution failed or timed out. | Use the reported stage and action, then review the generated handoff bundle or TODO template manually. |
| Local onboarding refuses existing scaffold files | A planned engine-generated file already exists and update mode was not selected. | Choose another directory or rerun with `--update-existing` after review. Existing prompts and skills remain preserved unless a separately reviewed prompt plan uses `--replace-consumer-owned`. |
| Onboarding warns about stale generated files | Files from an unselected deployment or prompt mode exist in the destination. | Review them manually. Onboarding leaves them untouched and never deletes them automatically. |
| `AI endpoint rejected tools` | The endpoint or model does not support OpenAI-style function calling. | Enable the provider's tool-call parser or choose a tool-capable model. |
| Zero jobs in `dashboard.json` | Discovery found no matches, or every discovered job failed while loading build data. | Check fetcher storage and artifact errors first, then validate the discovery selector. |
| Setup handoff warns that sampled builds have no JUnit | The selected jobs publish logs or another test format but no JUnit XML. | The engine can synthesize build-level failures, but test-level granularity is unavailable. Record the limitation during diagnostic authoring and use build-log and project-specific artifacts. |
| Pages workflow cannot find `project.yaml` | `project_dir` does not match the consumer layout. | Use `.` for the repository root or the exact subdirectory in the deploy workflow. |
| Dashboard assets return 404 | `branding.base_path` does not match the Pages repository. | Set it to `/<host-repo>` with no trailing slash. |
| Pages site is not deployed | Pages is not configured to use GitHub Actions. | Enable Pages with `gh api .../pages -X POST -F build_type=workflow`. |
| Private endpoint times out | The GitHub-hosted runner cannot reach the network. | Use Kubernetes-native mode, a self-hosted runner, or `skip-fetch` with committed data. |
| Analysis is generic | The project prompt lacks architecture, artifact layout, or real failure signatures. | Expand `prompts/system.md`. The update applies to new analyses; use an intentional cache rebaseline if existing entries must be replaced. |
| Cached analysis came from the old provider | Existing reusable entries retain their provider provenance after a provider change. | Set a new cache generation for a reversible full rebaseline. |
| `Propose fix` reports unavailable | The Agent Sandbox runtime is disabled, misconfigured, or its executor rejected the request. | Verify `agentSandbox.fixRuntime` Helm values, the executor image digest, provider Secret or gateway, allowed commands, and admission policy. |
| A Fix preview reports that the model provider rejected the sandbox credential | The provider answered the sandbox with 401 or 403, so no retry can succeed. | Fix the provider credential the sandbox uses, then request a new preview. The server log line for the request records the rejection, including the HTTP status when the provider reports one. |
| OAuth actions report that `BOT_TOKEN` is missing or cannot access a repository | OAuth identifies the admin, but the bot credential performs writes. | Add `BOT_TOKEN` to the OAuth auth Secret and scope it to the configured action repositories and operations. |
| Helm rejects `server.security.hsts.enabled=false` | HSTS cannot be disabled accidentally. | Keep HSTS enabled for deployments. For direct local HTTP only, set `server.development.allowInsecureHTTP=true`. |
| A confirmed bot write reports an audit persistence error | The external write may have succeeded, but the private audit could not be durably recorded. | Restore storage access and retry confirmation. The server reconciles the external result and records it as recovered before reporting success. |

## Useful checks

```bash
# Validate config and job discovery without AI.
./bin/aster -project-dir=../my-consumer -ai=false -builds=1

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

## Login fails with "oauth token exchange failed"

The server could not trade the authorization code for a token. The reason is
logged verbatim from GitHub:

```bash
kubectl -n <namespace> logs deploy/<release>-server | grep "OAuth token exchange"
```

`incorrect_client_credentials` usually means the stored credential carries a
stray newline. A Secret written with `echo` instead of `echo -n` keeps the
trailing byte, so a 40-character client secret arrives as 41 and GitHub rejects
it. Every binary inspects credential variables at startup and names each one
that carries surrounding whitespace:

```
⚠️  OAUTH_CLIENT_SECRET had leading or trailing whitespace; using the trimmed value.
⚠️  SESSION_KEY has leading or trailing whitespace and is used as written, ...
```

Fixed-format credentials (OAuth client credentials and bearer tokens) are
trimmed, because whitespace is never valid in them. Free-form secrets
(`SESSION_KEY`, `EMAIL_SMTP_PASSWORD`, `AUTH_PROXY_SECRET`) are reported but
used exactly as configured: trimming them could change a working value, and an
emptied `AUTH_PROXY_SECRET` would disable the shared-secret check outright.

A warning on any one variable means the whole Secret was probably written with
`echo`, so check every key. `BOT_TOKEN` is worth checking even when login works,
because it breaks the write actions rather than sign-in:

```bash
kubectl -n <namespace> get secret <auth-secret> \
  -o jsonpath='{.data.BOT_TOKEN}' | base64 -d | wc -c
```

Recreate the Secret with `printf %s` (not `echo`) to write the value cleanly.

A `403` with "oauth authorization did not grant the configured access" is a
different problem: the exchange succeeded but the grant was not exactly
`read:user`. See the section above.

## No jobs were published

A dashboard that loads with zero jobs has a valid frontend and manifest, but the
latest fetch published no job summaries. This has two common causes: discovery
found no matching jobs, or every discovered job failed while loading build data.

1. Check the fetcher logs first. `Warning: N jobs had fetch errors` and per-job
   errors point to storage connectivity, credentials, bucket routing, or malformed
   build data. Fix those errors before changing a valid discovery selector.
2. Run a one-build check without AI:

   ```bash
   ./bin/aster -project-dir=../my-consumer -ai=false -builds=1
   ```

3. Inspect the result:

   ```bash
   python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']))"
   ```

4. If the logs contain no job fetch errors, confirm `discovery.testgrid_dashboard` exactly
   matches the jobs' `testgrid-dashboards` annotation.
5. For broad bucket discovery, remove `discovery.job_filters` temporarily and
   confirm the storage provider, bucket, and gcsweb base. For
   `discovery.exact_jobs`, verify the exact case-sensitive job name and its
   direct `logs/<job>/` or `pr-logs/directory/<job>/` index.
6. Add `discovery.include_presubmits: true` to `project.yaml` only when the
   expected jobs are presubmits rather than periodics. Fetch commands do not
   provide a runtime override.

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
the dashboard calls the preview API. Fix proposals require an enabled Agent
Sandbox Fix runtime.


## Asynchronous draft stays pending or no ready email arrives

- Check the server logs and `GET /api/action-requests/<id>` status.
- A server restart marks an unfinished pending request failed because user tokens
  are intentionally never persisted. Start a new request from the pattern.
- Generic ready drafts persist for 24 hours in non-public
  `action_request_state.json`. Exact JUnit chat-to-fix previews use the
  confirmation token's 15-minute lifetime.
- Draft-ready email requires `EMAIL_SMTP_PASSWORD` in `server.extraEnv`, not only
  `fetcher.extraEnv`.
- The review link is bound to the authenticated login that created the request.


## Pull request triage stops updating

Symptom: `pull-requests.json` keeps an old `generated_at` while the rest of the
dashboard refreshes normally. The fetcher logs:

```
⚠ Pull request triage failed, keeping the previous view: ... 403 ...
```

The usual cause is anonymous GitHub reads. Triage calls the GitHub API on every
pass, and without a token GitHub caps the deployment at 60 requests per hour,
which one pass over a busy repository can spend on its own. A refresh failure
never aborts the pass, so the dashboard still publishes the previous view.

Look for this line in the startup logs, which is emitted once per process:

```
⚠ Pull request triage is enabled but neither GITHUB_READ_TOKEN nor GITHUB_TOKEN is set.
```

Fix it by setting `ai.githubReadToken` or `ai.githubReadTokenSecretName`, which
apply whether or not `ai.enabled` is true. For a public `branding.source_repo`
the token needs no repository privileges. `aster onboard doctor` reports the
same gap as `pull request triage credential`. The Pages path already receives
the Actions `GITHUB_TOKEN` from the reusable workflow, so this does not apply
there. See [Pull request triage](pull-request-triage.md).
