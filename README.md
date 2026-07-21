# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**: a project-agnostic
alternative to TestGrid with AI-driven failure analysis, run triage, and
notifications. Each project gets its own deployment, secrets, and GitHub Pages
or Kubernetes-native site without forking the engine.

> ⚠️ **Active development.** Engine APIs such as the `project.yaml` schema and
> reusable workflow inputs may still change. Pin to `@main` or a commit SHA
> until a stable release is cut. The moving `v1` alias does not exist before
> `v1.0.0` is published.

## How it works

The same engine ships **two deploy paths**; pick per project. Both build from
one codebase, run the same Go fetcher and React UI, and read the same
`project.yaml` + `prompts/system.md`.

**Kubernetes-native (in-cluster).** A worker runs the fetch and AI analysis
inside your cluster, next to the inference stack, and a small server serves the
dashboard plus the `/data/*.json` contract from a shared volume. The AI calls
stay in-cluster (low latency, no egress, private endpoints work), and it unlocks
interactive admin actions behind GitHub sign-in. File issue and Mark resolved
work in the standard image. Propose fix also requires `opencode` and git.
Deployed with the Helm chart in [`deploy/helm`](deploy/helm). The analysis
backend is selectable: the in-process agentic loop by default, or the advanced
experimental [Orka](experimental/orka/) Kubernetes-native pipeline. See
[Kubernetes deploy](docs/kubernetes.md) and [Server mode](docs/server.md).

**GitHub Actions + Pages (static).** A small reusable workflow runs the
fetcher on a schedule, builds the branded SPA, and publishes to the host repo's
GitHub Pages. Public, cheap, no backend or cluster. Read-only (no interactive
actions).

```yaml
# <your-repo>/.github/workflows/deploy.yml  (Pages path)
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: .          # wherever project.yaml lives
      ai-model: ${{ vars.AI_MODEL }}
      ai-endpoint: ${{ vars.AI_ENDPOINT }}
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      # Optional after notifications.email is enabled in project.yaml.
      EMAIL_SMTP_PASSWORD: ${{ secrets.EMAIL_SMTP_PASSWORD }}
```

Both paths serve the identical `/data/*.json` schema; the Kubernetes-native
server is a strict superset that adds a capability descriptor the frontend uses
to light up server-only features. Whichever you pick, you never fork the engine.

| | Kubernetes-native | GitHub Actions + Pages |
| --- | --- | --- |
| Runs the fetch | In-cluster worker/CronJob | GitHub Actions runner |
| Serves the site | In-cluster server (+ ingress) | GitHub Pages |
| AI endpoint | In-cluster or public | Public, self-hosted runner, or pre-fetched data |
| Interactive actions | Yes (admin sign-in) | No (read-only) |
| Needs a cluster | Yes | No |

## What you configure

A dashboard is shaped by three things:

- **`project.yaml`**: bucket, dashboard, branding, AI provider, and feature
  toggles. See [`configs/example/project.yaml`](configs/example/project.yaml)
  for the minimal required set and
  [`project.reference.yaml`](configs/example/project.reference.yaml) for the
  annotated field reference.
- **`prompts/system.md`**: project-specific AI knowledge. Mandatory; the fetcher
  hard-errors if it is missing when `-ai` is enabled.
- **The engine**, a Go fetcher in `backend/` and a React UI in `frontend/`, is
  built or imaged per project at deploy time; you never fork it.

## Documentation

**Getting started**
- [Onboarding a new project](docs/onboarding-a-new-project.md): choose a deploy
  path and scaffold it with the `onboard` subcommand.
- [GitHub Actions and Pages](docs/github-pages.md): configure and validate the
  static deployment.
- [Kubernetes-native](docs/kubernetes.md): deploy the worker and server with
  Helm.

**Configuration & authoring**
- [Project configuration](docs/project-configuration.md): strict
  `project.yaml` field reference and examples.
- [AI providers](docs/ai-providers.md): point the engine at any
  OpenAI-compatible endpoint, such as Copilot, OpenAI, Dynamo/NIM, vLLM, or
  Ollama.
- [Writing prompts](docs/writing-prompts.md): author the required
  `prompts/system.md`.
- [Agentic loop](docs/agentic.md): how the model browses artifacts via
  function-calling tools, and how to tune it per model tier.

**Features**
- [GitHub issues](docs/github-issues.md): auto-file and maintain issues for the
  highest-signal failures.
- [Skills](docs/skills.md): author diagnostic recipes for recurring patterns.
- [Fix PRs](docs/fix-prs.md): draft a minimal code fix for a recurring failure
  and open a guardrailed draft PR against the source repo.
- [Email notifications](docs/notifications.md): alert on persistent failures,
  changed errors, recoveries, and optionally link systemic patterns into the
  authenticated issue and fix review flow.

**Operations**
- [Kubernetes deploy](docs/kubernetes.md): run the dashboard in-cluster, a
  worker/CronJob writing to a shared volume that a server reads, via the Helm
  chart in `deploy/helm`.
- [Server mode](docs/server.md): the in-cluster server that serves the same
  `/data/*.json` contract plus a capability descriptor and admin-gated actions.
- [Orka quickstart](experimental/orka/QUICKSTART.md): set up the advanced,
  experimental Kubernetes-native analysis backend.
- [Orka worker compatibility](experimental/orka/worker-patches/COMPATIBILITY.md):
  pinned Orka source, worker image tags, digests, and validation contract.
- [Safe Orka evaluation](docs/orka-evaluation.md): run a side-effect-free,
  blue-green comparison with a fresh PVC and reversible cutover.
- [Troubleshooting](docs/troubleshooting.md): common first-deploy failures and
  checks.
- [Releasing](docs/releasing.md): cut an engine release and how consumers pin.

**Development**
- [Contributing](CONTRIBUTING.md): prerequisites and contribution workflow.
- [Local development](docs/development.md): build, test, and run the fetcher
  against a consumer repo locally.
- [Testing](docs/testing.md): unit, end-to-end pipeline, and quality-evaluation
  layers.

## Adding a project

See [onboarding](docs/onboarding-a-new-project.md). The `onboard` subcommand
scaffolds either path:

- **Kubernetes-native**: `onboard -mode k8s` generates `project.yaml`,
  `prompts/system.md`, and a `deploy/` folder (Helm `values.yaml` + a deploy
  README). Fill in the RWX storage class and AI endpoint, then `helm install`.
- **GitHub Actions + Pages**: `onboard` (default) generates `project.yaml`,
  `prompts/system.md`, and the two workflow files. Set the `AI_ENDPOINT` and
  `AI_MODEL` repository variables, set the `AI_TOKEN` secret, and enable Pages
  with **Source: GitHub Actions**.

Either way: no engine fork, no engine PR.

## License

[Apache 2.0](LICENSE)
