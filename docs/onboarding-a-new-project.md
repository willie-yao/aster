# Onboarding a project

The `fetcher onboard` command verifies Prow job discovery and creates a small
consumer scaffold. The project schema supports both analysis backends, while the
command sets deployment-specific branding and files for Pages or Kubernetes.

## Choose the environment

| Requirement | Deployment | Analysis |
| --- | --- | --- |
| Fastest evaluation or public read-only site | GitHub Actions and Pages | In-process |
| Private in-cluster model endpoint | Kubernetes with Helm | In-process initially |
| Authenticated server actions | Kubernetes with Helm | In-process or Orka |
| Per-failure Tasks and Orka observability | Kubernetes with Helm | Orka preview |

Orka is the strategic Kubernetes orchestration backend. It currently requires a
compatible Orka installation, Provider, and worker. The intended product
experience is for the dashboard deployment to manage those dependencies. The
first Kubernetes scaffold uses in-process analysis so it works by itself.

## What the scaffold creates

### GitHub Actions and Pages

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml
CHECKLIST.md
```

### Kubernetes with Helm

```text
project.yaml
prompts/system.md
deploy/values.yaml
deploy/README.md
```

Optional email, issue, fix-PR, action, and Orka configuration is not included in
the first-run scaffold. Add those features after the first dashboard works.

## Before you start

Collect:

- The TestGrid dashboard name, or an artifact bucket for bucket discovery.
- The dashboard repository as `owner/name`.
- The source repository under test as `owner/name`.
- An OpenAI-compatible endpoint, model id, and token when AI is enabled.

The model must support function calling.

## Fast start: scaffold it with `onboard`

Pages is the default and needs no engine checkout:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -out ./my-dashboard
```

For Kubernetes, add `-mode k8s`:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -mode k8s \
  -out ./my-dashboard
```

The command performs a real discovery sweep before it writes anything. A zero-job
result is an error rather than a broken scaffold.

### Other Prow installations

Replace `-testgrid` with `-bucket` to discover jobs from the artifact store:

```bash
-bucket "<bucket>"
```

Add `-gcsweb-base "https://gcsweb.example.net/s3"` when the bucket is served
through gcsweb.

### Optional prompt drafting

Without an AI token, `onboard` writes a short prompt with TODOs. To draft it from
the source repository documentation, set all three provider values before
running the command:

```bash
export AI_ENDPOINT="https://provider.example/v1/chat/completions"
export AI_MODEL="model-id"
export AI_TOKEN="token"
```

Always review an AI-drafted prompt before deployment.

### Open a scaffold pull request

Set `GITHUB_TOKEN` and use `-open-pr` instead of `-out`. The token needs contents
and pull-request write access to the dashboard repository.

## Review only four things

1. Confirm `project.yaml` discovery, storage, branding, and source repository.
2. Review or remove the inferred categories.
3. Replace the TODOs or verify the claims in `prompts/system.md`.
4. Follow `CHECKLIST.md` for Pages or `deploy/README.md` for Helm.

Do not add AI tuning, notifications, write features, or Orka settings until the
first fetch publishes the expected jobs.

## Deploy

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes with Helm](kubernetes.md)
- [Orka preview quickstart](../experimental/orka/QUICKSTART.md)

The Pages workflow uses repository variables `AI_ENDPOINT` and `AI_MODEL` plus
the `AI_TOKEN` secret. The Kubernetes scaffold stores endpoint and model in
`deploy/values.yaml` and accepts the token at install time.

## Validate the first result

A successful first deployment has:

- The expected project branding in `data/manifest.json`.
- At least one discovered job in `data/dashboard.json`.
- Grounded analysis on a failing test when AI is enabled.
- `/healthz` and `/api/capabilities` in Kubernetes mode.

If the dashboard has no jobs, use the discovery check in
[Troubleshooting](troubleshooting.md#no-jobs-were-published).

## Versioning and pinning

The workflow or image ref controls the engine version. Before the first stable
release, use `main`, a commit SHA, or an exact published prerelease. The moving
`v1` alias is created only after `v1.0.0` is published. See
[Releasing](releasing.md).

## Advanced options

`onboard -help` lists identity overrides, presubmit discovery, engine pinning,
bucket discovery, and pull-request creation. These are not required for a normal
first run.

The reusable Pages workflow also exposes build depth, worker count, runner,
timeout, pre-fetched data, and presubmit overrides. Keep their defaults until a
real deployment needs different operational behavior.
