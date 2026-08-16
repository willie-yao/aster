<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/aster-banner-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/aster-banner-light.svg">
    <img src="docs/assets/aster-banner-light.svg" alt="Aster. Turn failing tests into clear next steps.">
  </picture>
</p>

Aster is an evidence-first failure analysis and guarded-remediation engine for
Prow and Kubernetes test infrastructure. It watches Prow and TestGrid jobs,
investigates failures through bounded logs, test results, artifacts, history,
and source evidence, and helps maintainers move from signal to explanation to a
reviewed next step.

## Who Aster is for

Aster is for maintainers and platform teams that already operate Prow jobs, or
publish compatible job artifacts, and want a shared failure-analysis dashboard.
It supports a public, read-only GitHub Pages deployment and a Kubernetes
deployment with persistent data, authentication, chat, and guarded actions.

## What Prow means here

[Prow](https://docs.prow.k8s.io/docs/overview/) is Kubernetes-oriented CI and
job infrastructure used to trigger, run, and report repository tests. Aster
does not install Prow. It consumes Prow and TestGrid job configuration and build
artifacts, so onboarding requires existing Prow jobs or compatible artifacts in
a publicly readable GCS bucket.

## Prerequisites

Requirements depend on how you run and deploy Aster:

- The released `aster` CLI does not require an Aster source checkout.
- The `go run` path requires the Go version supported by this repository
  (currently Go 1.25).
- `gh` is needed only for GitHub-assisted repository, workflow, variable, or
  Secret operations.
- Node.js is needed only for frontend source development.
- An AI endpoint and credential are needed only when AI analysis is enabled.
- Kubernetes, Helm, and Flux are needed only for their respective deployment
  paths. GitHub Pages needs none of them.

## Quickstart

From a checkout of the repository whose jobs you want to monitor, run the
guided wizard at an exact released version:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 onboard \
  -engine-ref v0.9.0-rc.2
```

The wizard discovers matching Prow jobs, reviews deployment and AI choices,
validates the complete plan, and writes a small consumer repository only after
confirmation. The explicit engine ref pins a generated Pages workflow to the
same exact release. It does not require an Aster source checkout.

Continue with [Onboarding a project](docs/onboarding-a-new-project.md). Flagged,
dry-run, pull-request, and non-interactive usage is in the
[onboarding reference](docs/onboarding-reference.md). An LLM CLI can run the
same engine-owned workflow with `$setup-aster-consumer`; see the
[agent-driven setup guide](docs/agent-onboarding.md).

## Choose a deployment

| Need | Use |
| --- | --- |
| Fast evaluation or a public read-only dashboard | [GitHub Actions and Pages](docs/github-pages.md) |
| A private in-cluster model endpoint or persistent shared data | [Kubernetes with Helm](docs/kubernetes.md) or [Flux GitOps](docs/kubernetes-gitops.md) |
| Authenticated chat, File Issue, or Mark Resolved | [Kubernetes with Helm](docs/kubernetes.md) |
| No cluster to operate | [GitHub Actions and Pages](docs/github-pages.md) |

Both deployment paths use the supported in-process analyzer. Pages publishes
static JSON and assets. Kubernetes adds a server for authentication, chat, and
guarded actions. Experimental external runtimes and Fix PR generation are not
part of standard onboarding.

## Documentation

- [Onboarding](docs/onboarding-a-new-project.md)
- [GitHub Pages](docs/github-pages.md)
- [Kubernetes](docs/kubernetes.md)
- [Flux GitOps](docs/kubernetes-gitops.md)
- [Project configuration](docs/project-configuration.md)
- [Optional features](docs/optional-features.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Complete documentation map and contributor guides](docs/README.md)

## Name and mark

**Aster** stands for **Automated Signal Triage, Explanation, and Remediation**.
The mark combines a capital A, a forward-facing prow, and a central star,
representing a useful signal and a clear, reviewed next action.

## License

[Apache License 2.0](LICENSE)
