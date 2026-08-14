<p align="center">
  <img src="docs/assets/aster-mark.svg" alt="Aster logo" width="80" height="80">
</p>

# Aster

Reusable engine for AI-powered Prow and TestGrid dashboards. It discovers Prow
jobs, analyzes failures, renders a React dashboard, and can notify maintainers or
open guarded GitHub actions without requiring each project to fork the engine.

> **Active development.** Pin consumers to `@main`, a commit SHA, or an exact
> prerelease until a stable release and moving `v1` alias are published.

## Start here

Run the guided onboarding wizard from the source repository you want to monitor:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@latest onboard
```

The wizard detects the current GitHub repository where possible and walks you
through Prow discovery, deployment, AI, and output choices. It validates the
result before writing a small consumer repository.

Continue with [Onboarding a project](docs/onboarding-a-new-project.md). Flagged,
dry-run, pull-request, and non-interactive usage is in the
[onboarding reference](docs/onboarding-reference.md).

An LLM CLI can run the same engine-owned workflow with
`$setup-aster-consumer`. See the
[agent-driven setup guide](docs/agent-onboarding.md).

## Choose a deployment

| Need | Use |
| --- | --- |
| Fast evaluation or a public read-only dashboard | [GitHub Actions and Pages](docs/github-pages.md) |
| A private in-cluster model endpoint or persistent shared data | [Kubernetes with Helm](docs/kubernetes.md) |
| Authenticated chat, File Issue, or Mark Resolved | [Kubernetes with Helm](docs/kubernetes.md) |
| No cluster to operate | [GitHub Actions and Pages](docs/github-pages.md) |

Both deployment paths use the dashboard-owned in-process analyzer. It is the
supported and recommended runtime. Pages publishes static JSON and assets.
Kubernetes adds a server for authentication, chat, and guarded actions.

Experimental external runtimes and Fix PR generation are not part of standard
onboarding. Maintainers evaluating them can start from the
[complete documentation map](docs/README.md#experimental-features-and-runtimes).

## What a project owns

A consumer normally contains only:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml   # GitHub Pages
# or
deploy/values.yaml             # Kubernetes
```

- **`project.yaml`** identifies jobs, storage, branding, analysis policy, and
  optional features. Start with guided onboarding or the
  [configuration reference](docs/project-configuration.md).
- **`prompts/system.md`** supplies project-specific architecture, artifact, and
  failure knowledge. It is required when AI analysis is enabled.
- **Deployment configuration** supplies infrastructure details such as runner
  selection, model credentials, persistence, and authenticated server settings.

Aster preserves existing compatibility identifiers where changing them would
strand deployed consumers or persisted state. This includes `project.yaml` and
Helm value keys, JSON and state filenames, `prow-ai-dashboard/*` Kubernetes
labels and annotations, `PROW_AI_*` environment variables, action deduplication
markers, browser storage keys, and existing Helm release names.

The files under [`configs/example`](configs/example) are references, not a
ready-to-deploy consumer. Replace every placeholder and validate the result with
`aster onboard doctor`.

## How data flows

```text
Prow job configuration and artifact storage
                  |
            fetcher or worker
                  |
       in-process analysis
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

- [Onboarding](docs/onboarding-a-new-project.md)
- [GitHub Pages](docs/github-pages.md)
- [Kubernetes](docs/kubernetes.md)
- [Project configuration](docs/project-configuration.md)
- [Optional features](docs/optional-features.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Complete documentation map and contributor guides](docs/README.md)

## License

[Apache License 2.0](LICENSE)
