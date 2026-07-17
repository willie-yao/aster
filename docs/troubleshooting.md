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
| Orka Tasks remain pending | Orka, the Provider, tool shim, worker patches, or RBAC is incomplete. | Follow the experimental Orka quickstart and inspect Task and ai-worker status. |

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
