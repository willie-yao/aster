# prow-ai-dashboard

Reusable engine for AI-powered Prow and TestGrid dashboards. It discovers Prow
jobs, analyzes failures, renders a React dashboard, and can notify maintainers or
open guarded GitHub actions without requiring each project to fork the engine.

> **Active development.** Pin consumers to `@main`, a commit SHA, or an exact
> prerelease until a stable release and moving `v1` alias are published.

## Start here

The fastest way to try the dashboard is the GitHub Actions and Pages scaffold:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -out ./my-dashboard
```

The command verifies discovery before writing a small consumer repository. It
generates `project.yaml`, `prompts/system.md`, one deploy workflow, and a short
checklist. Continue with [Onboarding a project](docs/onboarding-a-new-project.md).

## Choose a deployment

Deployment and analysis are separate choices.

### Deployment

| Need | Use |
| --- | --- |
| Fast evaluation or a public read-only dashboard | GitHub Actions and Pages |
| A private in-cluster model endpoint | Kubernetes with Helm |
| Authenticated issue, fix, or resolve actions | Kubernetes with Helm |
| No cluster to operate | GitHub Actions and Pages |

**GitHub Actions and Pages** runs the fetcher on a schedule, builds the SPA, and
publishes static JSON and assets. It is inexpensive and read-only.

**Kubernetes with Helm** runs a worker or CronJob beside a small API and SPA
server. Use it for private inference endpoints, persistent shared data, and
server-side actions. See [Kubernetes deployment](docs/kubernetes.md).

### Analysis backend

| Backend | Availability | Use it when |
| --- | --- | --- |
| In-process | Pages and Kubernetes | You want the self-contained default |
| Orka preview | Kubernetes | You want per-failure Tasks, retries, observability, and agent-runtime integration |

The in-process backend is canonical and is the safest first install. The
current `analysis: orka` preview is a frozen compatibility-v6 `type: ai` path.
A separate dashboard-owned `type: container` prototype is being evaluated as an
optional lifecycle backend and will be productized or removed after its
production gates are tested. Both Orka paths require a compatible Orka control
plane today. See the [analysis runtime ownership decision](docs/architecture-decisions/0001-analysis-runtime-ownership.md)
and [Orka preview](experimental/orka/README.md).

Both analysis choices publish the same `jobs/*.json` and dashboard UI.

## What a project owns

A consumer normally contains only:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml   # Pages
# or deploy/values.yaml        # Kubernetes
```

- **`project.yaml`** identifies jobs, storage, branding, and optional features.
  Start with [the minimal example](configs/example/project.yaml) and use the
  [configuration reference](docs/project-configuration.md) only when adding an
  optional field.
- **`prompts/system.md`** supplies project-specific architecture, artifact, and
  failure knowledge. It is required when AI analysis is enabled.
- **Deployment configuration** supplies infrastructure details such as runner
  selection, model credentials, persistence, and Orka settings.

Most project settings are shared across deployments and analysis backends.
`branding.base_path` and `branding.site_url` remain deployment-specific, and
`onboard` generates the correct values for Pages or Kubernetes. In-process and
Orka analysis within the same Kubernetes deployment use the same project file.

## How data flows

```text
Prow job configuration and artifact storage
                  |
            fetcher or worker
                  |
        in-process analysis or Orka
                  |
 dashboard.json, jobs/*.json, flakiness.json
                  |
       Pages or the Kubernetes server
                  |
             React dashboard
```

The Kubernetes server serves the same `/data/*.json` contract as Pages and adds
`/api/capabilities` for server-only features.

## Documentation

### Get started

- [Onboarding a project](docs/onboarding-a-new-project.md)
- [GitHub Actions and Pages](docs/github-pages.md)
- [Kubernetes deployment](docs/kubernetes.md)
- [Project configuration](docs/project-configuration.md)
- [Troubleshooting](docs/troubleshooting.md)

### Improve analysis

- [AI providers](docs/ai-providers.md)
- [Writing the project prompt](docs/writing-prompts.md)
- [Agentic analysis](docs/agentic.md)
- [Custom diagnostic skills](docs/skills.md)

### Optional features

- [Email notifications](docs/notifications.md)
- [GitHub issues](docs/github-issues.md)
- [Agent-proposed fix PRs](docs/fix-prs.md)
- [Server and authenticated actions](docs/server.md)

### Orka preview

- [Analysis runtime ownership decision](docs/architecture-decisions/0001-analysis-runtime-ownership.md)
- [Product status and constraints](experimental/orka/README.md)
- [Quickstart](experimental/orka/QUICKSTART.md)
- [Safe evaluation](experimental/orka/EVALUATION.md)
- [Architecture](experimental/orka/ARCHITECTURE.md)
- [Worker compatibility](experimental/orka/worker-patches/COMPATIBILITY.md)

### Development

- [Contributing](CONTRIBUTING.md)
- [Local development](docs/development.md)
- [Testing](docs/testing.md)
- [Releasing](docs/releasing.md)

## License

[Apache 2.0](LICENSE)
