# prow-ai-dashboard

Reusable engine for **AI-powered Prow/TestGrid dashboards**: a project-agnostic
alternative to TestGrid with AI-driven failure analysis, run triage, and
notifications. Each project gets its own deployment, secrets, and GitHub Pages
site by calling the reusable workflow shipped here from any repo it controls.

> ⚠️ **Active development.** Engine APIs such as the `project.yaml` schema and
> reusable workflow inputs may still change. Pin to `@main` or a commit SHA
> until a release is cut.

## How it works

The same engine ships **two deploy paths**; pick per project. Both build from
one codebase, run the same Go fetcher and React UI, and read the same
`project.yaml` + `prompts/system.md`.

**Kubernetes-native (in-cluster).** A worker runs the fetch and AI analysis
inside your cluster, next to the inference stack, and a small server serves the
dashboard plus the `/data/*.json` contract from a shared volume. The AI calls
stay in-cluster (low latency, no egress, private endpoints work), and it unlocks
interactive admin actions (File issue / Propose fix) behind GitHub sign-in.
Deployed with the Helm chart in [`deploy/helm`](deploy/helm). The analysis
backend is selectable: the in-process agentic loop by default, or the
[Orka](experimental/orka/) Kubernetes-native pipeline, the recommended backend
once you run an in-cluster inference stack. See
[Kubernetes deploy](docs/kubernetes.md) and [Server mode](docs/server.md).

**GitHub Actions + Pages (static).** A ~20-line reusable workflow runs the
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
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      SLACK_WEBHOOK_URL: ${{ secrets.SLACK_WEBHOOK_URL }}
```

Both paths serve the identical `/data/*.json` schema; the Kubernetes-native
server is a strict superset that adds a capability descriptor the frontend uses
to light up server-only features. Whichever you pick, you never fork the engine.

| | Kubernetes-native | GitHub Actions + Pages |
| --- | --- | --- |
| Runs the fetch | In-cluster worker/CronJob | GitHub Actions runner |
| Serves the site | In-cluster server (+ ingress) | GitHub Pages |
| AI endpoint | In-cluster or public | Public (private via `skip-fetch`) |
| Interactive actions | Yes (admin sign-in) | No (read-only) |
| Needs a cluster | Yes | No |

## What you configure

A dashboard is shaped by three things:

- **`project.yaml`**: bucket, dashboard, branding, AI provider, and feature
  toggles. See [`configs/example/project.yaml`](configs/example/project.yaml)
  for the minimal required set and
  [`project.reference.yaml`](configs/example/project.reference.yaml) for every
  field.
- **`prompts/system.md`**: project-specific AI knowledge. Mandatory; the fetcher
  hard-errors if it is missing when `-ai` is enabled.
- **The engine**, a Go fetcher in `backend/` and a React UI in `frontend/`, is
  built or imaged per project at deploy time; you never fork it.

## Documentation

**Getting started**
- [Onboarding a new project](docs/onboarding-a-new-project.md): choose a deploy
  path, scaffold it with the `onboard` subcommand (`-mode k8s` or Pages), then a
  full field-by-field reference covers the rest.
- [Orka backend quickstart](experimental/orka/QUICKSTART.md): step-by-step setup for
  the recommended Kubernetes-native analysis backend, running failure analysis as
  Orka Tasks alongside your in-cluster inference stack. See also the
  [overview](experimental/orka/README.md) and
  [architecture](experimental/orka/ARCHITECTURE.md).

**Configuration & authoring**
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
- [Skills](docs/skills.md): author diagnostic recipes, and auto-suggest new
  ones for recurring patterns.
- [Fix PRs](docs/fix-prs.md): draft a minimal code fix for a recurring failure
  and open a guardrailed draft PR against the source repo.

**Operations**
- [Kubernetes deploy](docs/kubernetes.md): run the dashboard in-cluster, a
  worker/CronJob writing to a shared volume that a server reads, via the Helm
  chart in `deploy/helm`.
- [Server mode](docs/server.md): the in-cluster server that serves the same
  `/data/*.json` contract plus a capability descriptor and admin-gated actions.
- [Orka analysis backend](experimental/orka/ARCHITECTURE.md): how the recommended
  Kubernetes-native backend runs failure analysis as Orka Tasks alongside your
  inference stack (setup lives in the Getting started quickstart above).
- [Releasing](docs/releasing.md): cut an engine release and how consumers pin.

**Development**
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
  `prompts/system.md`, and the two workflow files. Set the `AI_TOKEN` secret and
  enable Pages with **Source: GitHub Actions**.

Either way: no engine fork, no engine PR.

## License

[Apache 2.0](LICENSE)
