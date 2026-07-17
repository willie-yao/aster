# Onboarding a new project

The `fetcher onboard` command discovers the project's Prow jobs and creates a
ready-to-review consumer scaffold. Use it for either GitHub Pages or the
Kubernetes-native deployment.

This page covers the common setup. Continue with the deployment guide for the
path you choose:

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes-native](kubernetes.md)

## Choose a deployment

| | GitHub Actions and Pages | Kubernetes-native |
| --- | --- | --- |
| Fetch location | GitHub Actions runner | Cluster worker or CronJob |
| Site hosting | GitHub Pages | Go server and Service or Ingress |
| Private model endpoint | Self-hosted runner or pre-fetched data | Native |
| Interactive actions | No | Optional |
| Extra infrastructure | None | Kubernetes and RWX storage |
| Analysis backend | In-process | In-process by default; experimental Orka option |

Both paths use the same `project.yaml`, `prompts/system.md`, frontend, and public
JSON contract.

## Files in a consumer

```text
# GitHub Pages
project.yaml
prompts/system.md
.github/workflows/deploy.yml
.github/workflows/clear-cache.yml
CHECKLIST.md

# Kubernetes-native
project.yaml
prompts/system.md
deploy/values.yaml
deploy/README.md
CHECKLIST.md
```

A Pages consumer may place `project.yaml` and `prompts/` in a subdirectory. The
workflow's `project_dir` must point to that directory.

## Before you start

Collect these values:

- The TestGrid dashboard name, or the artifact bucket for bucket discovery.
- The dashboard host repository as `owner/name`.
- The source repository under test as `owner/name`.
- An AI endpoint, model id, and bearer token if AI analysis will be enabled.
- Confirmation that the endpoint and model support OpenAI-style function
  calling.

The AI endpoint and model are deployment settings as well as prompt-drafting
settings. Supplying them while running `onboard` drafts the prompt, but a Pages
consumer must also save them as repository variables or commit them in
`project.yaml`.

## Fast start: scaffold it with `onboard`

The module command needs no checkout:

```bash
export GITHUB_TOKEN="$(gh auth token)"

# Optional prompt drafting. Omit all three or pass -no-prompt for a TODO draft.
export AI_ENDPOINT="https://provider.example/v1/chat/completions"
export AI_MODEL="model-id"
export AI_TOKEN="token"

go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -out ./my-dashboard
```

Pages is the default mode. Add `-mode k8s` for the Kubernetes-native layout:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -mode k8s \
  -out ./my-dashboard
```

For a Prow installation discovered from its artifact bucket, replace
`-testgrid` with:

```bash
-bucket "<bucket>"
```

Add `-gcsweb-base "https://gcsweb.example.net/s3"` when the bucket is behind a
gcsweb gateway.

To have the command open a scaffold PR instead of writing locally, replace
`-out` with `-open-pr`. The `GITHUB_TOKEN` then needs contents and pull-request
write access to the dashboard repository.

A local checkout works too:

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend
go build -o /tmp/fetcher ./cmd/fetcher
/tmp/fetcher onboard -testgrid ... -dashboard-repo ... -source-repo ... -out ./my-dashboard
```

## Review the generated files

The command validates discovery before writing and infers categories from job
names. Review every generated file before deployment:

1. Replace prompt TODOs and verify all drafted project claims.
2. Reorder, relabel, or remove inferred categories.
3. Confirm storage, branding, and source repository values.
4. Follow `CHECKLIST.md` for provider credentials and deployment steps.

The prompt is required only when AI analysis is enabled. A discovery-only fetch
with `-ai=false` does not load it.

## Confirm discovery manually

Use this when diagnosing a dashboard name or bucket before scaffolding:

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend
go build -o /tmp/fetcher ./cmd/fetcher

mkdir -p /tmp/sweep
cat > /tmp/sweep/project.yaml <<'YAML'
id: myproject
name: "My Project"
testgrid:
  dashboard: "<testgrid-dashboard>"
storage:
  provider: gcs
  bucket: kubernetes-ci-logs
branding:
  title: "My Project Prow Dashboard"
  base_path: "/my-dashboard"
  site_url: "https://my-org.github.io/my-dashboard"
  source_repo:
    owner: my-org
    name: myproject
YAML

GITHUB_TOKEN="$(gh auth token)" \
  /tmp/fetcher -project-dir=/tmp/sweep -ai=false -builds=1
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']), 'jobs')"
```

A nonzero count confirms the discovery selector. Add
`source.include_presubmits: true` if the project has only presubmit jobs.

## Configure the project

See [Project configuration](project-configuration.md) for the field reference and
[`project.reference.yaml`](../configs/example/project.reference.yaml) for a full
annotated example.

At minimum, confirm:

- `storage` identifies the artifact backend.
- `testgrid.dashboard` is correct, unless bucket discovery is used.
- `branding.base_path` matches the deployment.
- `branding.source_repo` points at the code repository under test.
- AI provider settings are supplied through YAML or the deployment environment.
- Optional email notification settings identify the SMTP relay and recipients.

## Write the project prompt

`prompts/system.md` supplies the architecture, artifact layout, known failure
patterns, and transient classes that the engine cannot infer generically. Review
[Writing prompts](writing-prompts.md) before the first AI run.

Prompt edits automatically invalidate affected cached analyses. A manual cache
clear is needed only when you want an immediate full rebaseline.

## Deploy

### Step 3a: Kubernetes-native

Continue with [Deploy Kubernetes-native](kubernetes.md).

### Step 3b: GitHub Actions and Pages

Continue with [Deploy with GitHub Actions and Pages](github-pages.md).

For an advanced Kubernetes-native analysis backend, see the
[experimental Orka quickstart](../experimental/orka/QUICKSTART.md). Orka requires
additional components and worker patches and is not the default scaffold.

## Versioning and pinning

The workflow or image ref controls the engine version. Before the first stable
release, use `main`, a commit SHA, or an exact published prerelease. The moving
`v1` alias is created only after `v1.0.0` exists. See
[Releasing](releasing.md) and the version section in the
[Pages guide](github-pages.md#engine-version).

## Validate the first deployment

Check these endpoints or files:

- `data/manifest.json` contains the expected branding.
- `data/dashboard.json` contains the discovered jobs.
- A failing test in `data/jobs/*.json` has grounded AI analysis.
- Kubernetes-native deployments return `ok` from `/healthz` and `mode: server`
  from `/api/capabilities`.

See [Troubleshooting](troubleshooting.md) for common first-run failures.
