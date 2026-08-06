# Consumer setup decisions

## Placement

| Choice | Use when | `-out` |
| --- | --- | --- |
| Current directory | The current repository will also own the consumer files | `.` |
| Existing checkout | A consumer repository already exists locally | Its checkout path |
| Subdirectory | A monorepo owns the consumer in a dedicated directory | The subdirectory |
| Separate directory | Source and consumer should remain independent | A sibling or explicit path |

Existing generated files require a reviewed dry run and explicit
`-update-existing`. Unrelated and stale files remain untouched.

## Deployment

| Mode | Use when | Generated deployment files |
| --- | --- | --- |
| `pages` | Public read-only dashboard and provider reachable from GitHub Actions | `.github/workflows/deploy.yml`, `CHECKLIST.md` |
| `k8s` | Private artifacts, cluster-local provider, persistent state, or authenticated actions | `deploy/values.yaml`, `deploy/README.md` |

Do not select Kubernetes merely because the source project uses Kubernetes.
Choose it only when the dashboard deployment requires it.

## Discovery

Use `-testgrid` for Kubernetes TestGrid dashboards. Use `-bucket` for a Prow
artifact bucket that is not discovered through TestGrid. Add `-gcsweb-base` only
when that bucket is served through a gcsweb gateway.

Postsubmit tabs may appear in discovery totals but are not ingested. Enable
`-include-presubmits` only when the selected dashboard requires presubmit
coverage and the user accepts the additional history and fetch cost.

## Prompt authoring

Use `-prompt-mode handoff` when the current agent will author the project prompt.
The scaffold provides the pinned source ref, matched Prow metadata, and the
engine-owned system-prompt-generation skill. Avoid spawning a nested local agent
unless the user explicitly selects `-prompt-mode agent` and the pinned sandboxed
OpenCode runtime is available.

## Deployed AI

Prompt authoring and deployed failure analysis are separate. `-ai=false` starts
with deployed analysis disabled. When AI is enabled, generated documentation
explains how to configure `AI_API`, `AI_ENDPOINT`, `AI_MODEL`, and `AI_TOKEN`.
The setup agent must not request or write the token value.

## Write boundaries

A request to set up the local consumer authorizes the reviewed scaffold write.
It does not automatically authorize Git initialization, GitHub repository
creation, pushes, pull requests, Pages configuration, Secret writes, Helm
installation, or deployment.
