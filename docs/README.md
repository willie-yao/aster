# Documentation

Choose the page for your task. First-time users should start with onboarding,
then follow one deployment guide.

## Start here

| Audience | Start page | Next reference |
| --- | --- | --- |
| Project contributor | [Onboarding a project](onboarding-a-new-project.md) | [Onboarding automation reference](onboarding-reference.md) |
| GitHub Pages operator | [GitHub Actions and Pages](github-pages.md) | [Project configuration](project-configuration.md) |
| Kubernetes contributor | [Kubernetes quickstart](kubernetes.md) | [Kubernetes operator reference](kubernetes-reference.md) |
| Platform administrator | [Kubernetes platform setup](kubernetes-platform.md) | [Flux GitOps deployment](kubernetes-gitops.md) |
| Aster contributor | [Development](development.md) | [Testing](testing.md) and [architecture](architecture/in-process-analyzer.md) |
| Aster maintainer | [Maintainer references](maintainer/README.md) | [Releasing](releasing.md) |

Use [Troubleshooting](troubleshooting.md) when an existing deployment is not
producing expected data.

## Configure analysis

- [Project configuration](project-configuration.md) is the schema reference.
- [Writing the project prompt](writing-prompts.md) owns prompt structure and
  cache-generation guidance.
- [AI providers](ai-providers.md) owns endpoint, model, credential, and protocol
  compatibility.
- [Agentic analysis](agentic.md) explains the authoritative tool and evidence
  loop, quality gates, cache behavior, and operations.
- [Diagnostic skills](skills.md) documents consumer-owned evidence recipes.

## Deploy

- [GitHub Actions and Pages](github-pages.md) is the public, read-only path.
- [Kubernetes quickstart](kubernetes.md) is the common in-cluster path.
- [Flux GitOps deployment](kubernetes-gitops.md) covers generated Flux bundles.
- [Kubernetes platform setup](kubernetes-platform.md) defines platform ownership,
  secure runtimes, networking, storage, and Secret boundaries.
- [Kubernetes operator reference](kubernetes-reference.md) contains detailed
  chart and lifecycle behavior.
- [Server mode](server.md) covers endpoints, authentication, chat, and guarded
  actions.

## Optional features

Core onboarding enables failure analysis only. Add optional features after the
basic dashboard is healthy.

| Feature | Canonical guide | Boundary |
| --- | --- | --- |
| Pull request triage | [Pull request triage](pull-request-triage.md) | Deterministic cross-deployment view of open pull request failures, with optional bot comments and authenticated AI escalation. |
| Analysis chat | [Server mode](server.md#analysis-chat) | Authenticated, read-only model conversation over published analysis. |
| Cause-scoped analysis chat | [Server mode](server.md#analysis-chat) | Authenticated chat over one causal group's failed builds and newest later completed comparison. |
| File Issue and Mark Resolved | [Server mode](server.md#admin-gated-actions) and [GitHub issues](github-issues.md) | Authenticated preview or lifecycle action. GitHub writes use a server-held `BOT_TOKEN`. |
| Email notifications | [Notifications](notifications.md) | SMTP credentials and routing stay deployment-owned. |
| Fix PR generation | [Fix PR generation](fix-prs.md) | Experimental, confirmation-gated code writing through Agent Sandbox. |

Recommended order:

1. Deploy the read-only dashboard and verify current jobs and analysis.
2. Enable pull request triage when maintainers need a repository-wide
   presubmit view.
3. Add authentication and analysis chat if maintainers need interactive review.
4. Add notifications, issue drafting, or resolution controls as separate needs.
5. Evaluate Fix PR generation only after the Agent Sandbox platform contract is
   installed and reviewed.

## Contribute and operate

- [Development](development.md)
- [Testing](testing.md)
- [Releasing](releasing.md)
- [Brand](brand.md)
- [Notifications](notifications.md)
- [GitHub issues](github-issues.md)
