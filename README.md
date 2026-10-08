<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/aster-banner-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/aster-banner-light.png">
    <img src="docs/assets/aster-banner-light.png" width="1280" alt="Aster. Turn failing tests into clear next steps. Automated Signal Triage, Explanation, and Remediation.">
  </picture>
</p>

## **Aster**: **Automated Signal Triage, Explanation, and Remediation**.

Aster is an evidence-first failure analysis and guarded-remediation engine for [Prow](https://docs.prow.k8s.io/docs/overview/) and Kubernetes test infrastructure. It watches Prow and TestGrid jobs, investigates failures through bounded logs, test results, artifacts, history, and source evidence, and helps maintainers move from signal to explanation to a reviewed next step.

## Who Aster is for

Aster is for maintainers and platform teams that already operate Prow jobs, or publish compatible job artifacts, and want a shared failure-analysis dashboard. It supports a public, read-only GitHub Pages deployment and a Kubernetes deployment with persistent data, authentication, chat, and guarded actions.

See the [public CAPZ dashboard](https://capz-aster-99e3d335-c8fva4csbpcuh2e8.b01.azurefd.net/) for a working Kubernetes deployment:

[![Aster showing a CAPZ test failure, its analysis, and artifact evidence](docs/assets/aster-dashboard-preview.png)](https://capz-aster-99e3d335-c8fva4csbpcuh2e8.b01.azurefd.net/)

## Before you start

- Have Go installed and a Git checkout of the repository whose jobs you want to monitor. Go can download the required toolchain automatically.
- Choose an existing GitHub repository you control for the dashboard configuration. It holds the generated files, not a copy of Aster's code.
- Your jobs need Prow-compatible artifacts. The wizard discovers Kubernetes test-infra jobs automatically; other Prow installations can use [bucket discovery](docs/onboarding-reference.md#non-interactive-automation).
- Have a Chat Completions or Responses endpoint and model that support function calling. Follow [AI provider configuration](docs/ai-providers.md) for the API, endpoint, model, and credential.

Pages needs public artifacts and an AI endpoint reachable from its Actions runner. Kubernetes needs a cluster with [shared ReadWriteMany storage](docs/kubernetes.md#prerequisites). You can configure GitHub through its UI or `gh`; `gh` is not required for the wizard.

## Quickstart

From a checkout of the repository whose jobs you want to monitor, run the guided wizard at an exact released version:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.11.0 onboard \
  -engine-ref v0.11.0
```

The wizard discovers matching Prow jobs, reviews deployment and AI choices, validates the complete plan, and writes a small consumer repository only after confirmation. The explicit engine ref pins a generated Pages workflow to the same exact release. It does not require an Aster source checkout.

Continue with [Onboarding a project](docs/onboarding-a-new-project.md). Flagged, dry-run, pull-request, and non-interactive usage is in the [onboarding reference](docs/onboarding-reference.md). A coding agent can run the same engine-owned workflow with `$setup-aster-consumer`; installation and safety boundaries are in the onboarding quickstart.

## Choose a deployment

| Need | Use |
| --- | --- |
| Fast evaluation or a public read-only dashboard | [GitHub Actions and Pages](docs/github-pages.md) |
| A private in-cluster model endpoint or persistent shared data | [Kubernetes with Helm](docs/kubernetes.md) or [Flux GitOps](docs/kubernetes-gitops.md) |
| Authenticated chat, File Issue, or Mark Resolved | [Kubernetes with Helm](docs/kubernetes.md) |
| No cluster to operate | [GitHub Actions and Pages](docs/github-pages.md) |

Both deployment paths use the supported in-process analyzer. Pages publishes static JSON and assets. Kubernetes adds a server for authentication, chat, and guarded actions. Nothing files an issue or opens a pull request on a schedule; both are maintainer-confirmed actions. The scheduled pass has only two GitHub write paths, each opt-in and off by default: recovery on an issue it already tracks, which comments and optionally closes, and the bot comment on a newly opened pull request. Fix PR generation is not part of standard onboarding.

## Documentation

- [Onboarding](docs/onboarding-a-new-project.md)
- [GitHub Pages](docs/github-pages.md)
- [Kubernetes](docs/kubernetes.md)
- [Flux GitOps](docs/kubernetes-gitops.md)
- [Project configuration](docs/project-configuration.md)
- [Pull request triage](docs/pull-request-triage.md)
- [Optional features and enablement order](docs/README.md#optional-features)
- [Troubleshooting](docs/troubleshooting.md)
- [Complete documentation map and contributor guides](docs/README.md)

## License

[Apache License 2.0](LICENSE)
