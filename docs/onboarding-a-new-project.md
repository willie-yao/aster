# Onboarding a new project

The fastest way to add a prow-ai-dashboard project is the [`onboard`
subcommand](#fast-start-scaffold-it-with-onboard) below: it scaffolds the whole
file set for either deploy path. The rest of this page is the full reference for
the fields and steps it generates.

## Choose your path

The engine ships two deploy paths from one codebase. Pick per project; both use
the same `project.yaml` and `prompts/system.md`, and you never fork the engine.

- **Kubernetes-native** (this page covers it [first](#step-3-deploy)). A worker
  runs the fetch and AI analysis in your cluster and a server serves the
  dashboard from a shared volume. Choose it when the model runs in-cluster (or
  you want analysis to stay in-cluster), or when you want the interactive admin
  actions (File issue / Propose fix, which are Kubernetes-native only). Needs a
  cluster with a `ReadWriteMany` storage class.
- **GitHub Actions + Pages** (covered [second](#step-3b-github-actions--pages)).
  A reusable workflow runs the fetcher on a schedule and publishes a static site
  to GitHub Pages. Choose it for a public, backend-free dashboard with no
  cluster. Read-only. The host repo must not already publish a Pages site.

`onboard` scaffolds either: `-mode k8s` for the Kubernetes-native layout,
`-mode pages` (the default) for the Pages layout.

## What you ship

A dashboard is a few small files in a **dedicated repo** or a **subdirectory of
an existing repo**. Everything else is reused from the engine at deploy time.
The config and prompt are the same for both paths; only the deploy files differ.

```
# Kubernetes-native (-mode k8s)          # GitHub Actions + Pages (default)
<repo>/                                  <host-repo>/
├── project.yaml                         ├── <project_dir>/
├── prompts/system.md                    │   ├── project.yaml
└── deploy/                              │   └── prompts/system.md
    ├── values.yaml   # Helm values      └── .github/workflows/
    └── README.md     # install steps        ├── deploy.yml
                                              └── clear-cache.yml
```

- `project_dir` (Pages) points at wherever `project.yaml` lives: `.` for a
  dedicated repo, or a subdir such as `dashboard/`.
- No Go or React code, and no engine fork.
- Both **periodic** and **presubmit** jobs are supported. Periodics are on by
  default; enable presubmits with `source.include_presubmits: true` in
  `project.yaml` (or `include-presubmits: true` on the reusable workflow).

## Fast start: scaffold it with `onboard`

The `onboard` subcommand generates this whole file set for you. It verifies your
discovery config actually finds jobs, infers `categories` from the job names,
and produces a ready-to-review scaffold plus a `CHECKLIST.md` of the manual
steps. It never touches secrets. `-mode` selects the deploy path:

- `-mode k8s`: `project.yaml`, a `prompts/system.md` draft, and a `deploy/`
  folder (Helm `values.yaml` + a deploy README). When `AI_ENDPOINT` / `AI_MODEL`
  are set (below), they seed `deploy/values.yaml` so the install is ready to run.
- `-mode pages` (default): `project.yaml`, a `prompts/system.md` draft, and the
  two workflow files (`deploy.yml`, `clear-cache.yml`).

There are two ways to run it, from least to most setup.

### Option A: one command, no clone (`go run` from the module)

```bash
export GITHUB_TOKEN=$(gh auth token)   # reads the source repo's docs + your jobs
# Optional: drafts prompts/system.md. If you set AI_TOKEN, you must also set
# AI_ENDPOINT and AI_MODEL for your chat-completions provider (the engine assumes
# no default endpoint); see docs/ai-providers.md. Omit all three to write a stub.
export AI_TOKEN=...
export AI_ENDPOINT="https://<your-provider>/chat/completions"
export AI_MODEL="<your-model-id>"
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<your-testgrid-dashboard-name>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<code-repo-under-test>" \
  -mode k8s \
  -out ./my-dashboard
```

Drop `-mode k8s` (or pass `-mode pages`) for the GitHub Actions + Pages layout.
This writes a local scaffold directory you review and push yourself. To skip the
local copy and have onboard open the PR for you, swap `-out` for `-open-pr`. It
uses `GITHUB_TOKEN` for write access to the dashboard repo (needs
`contents:write` + `pull-requests:write`) and lands all files in one commit on a
new branch:

```bash
go run github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@latest onboard \
  -testgrid "<your-testgrid-dashboard-name>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<code-repo-under-test>" \
  -open-pr
```

For a non-kubernetes Prow (a project-dedicated bucket, optionally behind
gcsweb), swap the discovery selector in either command:

```bash
  -bucket "<bucket>" [-gcsweb-base "https://gcsweb.<project>.io/s3"] \
```

### Option B: clone and build

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher
/tmp/fetcher onboard -testgrid ... -dashboard-repo ... -source-repo ... -out ./my-dashboard
```

**The `prompts/system.md` draft.** When `AI_TOKEN` is set (the credential for
your chat-completions endpoint, alongside the required `AI_ENDPOINT` and
`AI_MODEL` for your provider, same as the fetcher; see
[ai-providers.md](ai-providers.md)), onboard reads the source repo's own docs
(README, `docs/`, architecture/contributing material) and drafts a real
`prompts/system.md` grounded in them: the architecture, where evidence lives, and
known transient classes. Without a token (or with `-no-prompt`), it writes a stub
with TODOs instead. Either way the result is **a draft to review**, not a
finished prompt; prompt quality is the biggest lever on analysis depth.

Then **review the output**: refine `prompts/system.md`, trim or reorder the
inferred `categories`, and follow `CHECKLIST.md` for the remaining steps (for
Pages: enable Pages and set the `AI_TOKEN` secret; for Kubernetes-native: fill in
`deploy/values.yaml` and `helm install`). The sections below are the manual
reference for each generated field if you prefer to write them by hand or to
understand what the scaffold produced.

## Step 0: sweep the jobs first

Confirm the engine discovers your jobs before writing the full config.

```bash
git clone https://github.com/willie-yao/prow-ai-dashboard /tmp/engine
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher

mkdir -p /tmp/sweep/prompts && cd /tmp/sweep
cat > project.yaml <<'YAML'
id: myproject
name: "My Project"
testgrid:
  dashboard: "<your-testgrid-dashboard-name>"   # e.g. "sig-release-master-blocking"
storage:
  provider: gcs
  bucket: "kubernetes-ci-logs"
branding:
  title: "My Project Prow Dashboard"
  base_path: "/<repo-name>"
  site_url: "https://my-org.github.io/<repo-name>"
  source_repo:
    owner: "<org>"
    name: "<repo>"
YAML
echo "# placeholder" > prompts/system.md     # required; empty fails
export GITHUB_TOKEN=$(gh auth token)          # avoids the 60/hr anonymous limit
/tmp/fetcher -project-dir=. -ai=false -builds=1
python3 -c "import json; print(len(json.load(open('data/dashboard.json'))['jobs']), 'jobs')"
```

A non-zero count confirms `testgrid.dashboard` is correct. Zero means the name
does not match any job's `testgrid-dashboards` annotation; fix it before
continuing. If your project has only presubmit jobs, add `source:` with
`include_presubmits: true` to the sweep config (periodics are the default).

## Step 1: `project.yaml`

Start from [`configs/example/project.yaml`](../configs/example/project.yaml),
which annotates every field. The fields that matter:

- **`storage`** (required): where the project's Prow build artifacts live. The
  engine does not assume GCS.
  - `provider` (required): the storage backend. `gcs` for native Google Cloud
    Storage (kubernetes.io Prow), or `gcsweb` for any gcsweb HTTP gateway
    fronting a bucket (e.g. an S3 bucket behind `gcsweb.<project>.io`).
  - `bucket`: the bucket name, no `gs://`/`s3://` prefix, e.g. `kubernetes-ci-logs`.
  - For `gcsweb`, also set `base` (the gateway root that serves raw objects and
    HTML listings, e.g. `https://gcsweb.istio.io/s3`) and usually `prow_base`
    (the Prow deck root, e.g. `https://prow.istio.io/view/s3`).
- **`discovery`** (optional): how the fetcher finds the project's jobs.
  - `source: testgrid` (default): kubernetes/test-infra job YAMLs filtered by
    `testgrid.dashboard`. The kubernetes ecosystem path.
  - `source: bucket`: list the storage bucket's own job indexes (`logs/` and
    `pr-logs/directory/`). Works for any Prow instance regardless of where job
    configs live; no `testgrid.dashboard` needed. Optional `job_filters` keep
    only job names containing a substring (omit to take every job in the
    bucket, which suits a project-dedicated bucket).
- **`testgrid.dashboard`** (required only for `discovery.source: testgrid`): the
  value your jobs set in their `testgrid-dashboards` annotation, e.g.
  `"sig-release-master-blocking"`. The engine keeps every job whose annotation
  contains this name, regardless of the file path the job is defined in. Find it
  in the job's definition under `kubernetes/test-infra/config/jobs/`, or on
  testgrid. Release-branch periodics that advertise a different dashboard are
  excluded automatically.
- **`branding`** (required):
  - `title`: header text, e.g. `"My Project Prow Dashboard"`.
  - `base_path`: where the SPA is served. For Pages it is the sub-path `/` plus
    the repo that serves Pages, e.g. `/myproject-prow-ai-dashboard` (leading
    slash, no trailing slash). For Kubernetes-native the server serves at the
    domain root, so use `/`.
  - `site_url`: the full dashboard URL. For Pages, e.g.
    `https://my-org.github.io/myproject-prow-ai-dashboard`. For Kubernetes-native
    use your ingress hostname, or a placeholder such as `http://localhost:8080`
    until one exists (it is only used for the dashboard link in issue/PR bodies
    and the UI).
  - `source_repo`: the project's code repo, as `{owner, name}`, e.g.
    `{owner: kubernetes, name: kubernetes}`. Used to link cited source files;
    this is the code repo, not the dashboard repo.
- **`categories`** (optional): an ordered list of `{match, id, label}`, e.g.
  `{match: "conformance", id: "conformance", label: "Conformance"}`. `match` is
  a lowercase substring tested against the job name; first match wins; unmatched
  jobs go in `"other"`. Put specific rules before broad ones. Omit it to render
  one flat grid. `category_display_order` (a list of `id`s) orders the sections
  independently of match precedence.
- **`ai`** (optional): the endpoint, model, and tools for the analysis loop.
  When AI is enabled, `endpoint` and `model` are required (the engine has no
  default provider); set them here or via the `AI_ENDPOINT` / `AI_MODEL` env
  vars, e.g. `endpoint: "https://api.githubcopilot.com/chat/completions"`,
  `model: "claude-sonnet-4.5"`, `tools: [filesystem, k8s]`. All other knobs have
  defaults. See [agentic.md](agentic.md) for the full schema and
  [ai-providers.md](ai-providers.md) for provider-specific endpoint and model
  values.

### Example: a non-GCS Prow (S3 behind gcsweb)

A project on its own Prow instance, with artifacts in S3 fronted by a gcsweb
gateway and no testgrid annotations, uses the `gcsweb` provider plus bucket
discovery:

```yaml
storage:
  provider: gcsweb
  bucket: my-prow
  base: "https://gcsweb.my-project.io/s3"     # raw objects + HTML listings
  prow_base: "https://prow.my-project.io/view/s3"
discovery:
  source: bucket
  job_filters:
    - "integ-"        # keep only jobs whose name contains this (omit for all)
```

Everything downstream (build enumeration, JUnit parsing, the agentic analysis)
is identical; only the storage and discovery blocks change.

## Step 2: `prompts/system.md`

Required: the fetcher hard-errors at startup if it is missing or
whitespace-only. There is no default prompt. See
[writing-prompts.md](writing-prompts.md) for the sections worth including.

## Step 3: deploy

Pick the path you chose above. Kubernetes-native is covered first, GitHub Actions
+ Pages second.

### Step 3a: Kubernetes-native

`onboard -mode k8s` writes a `deploy/` folder with a Helm `values.yaml` and a
`README.md`. If you are writing it by hand, `deploy/values.yaml` supplies the
engine chart at `willie-yao/prow-ai-dashboard/deploy/helm/prow-ai-dashboard`;
`project.yaml` and `prompts/system.md` are passed separately at install time.

**Prerequisites.**

- A cluster with an OpenAI-compatible chat-completions endpoint reachable over
  cluster DNS (or a public one). See [ai-providers.md](ai-providers.md).
- A `ReadWriteMany` storage class: the worker (writer) and server (reader) mount
  one shared volume at once. Examples: `azurefile-csi` (AKS), `efs-sc` (EKS),
  `filestore`/`nfs` (GKE).
- A checkout of `willie-yao/prow-ai-dashboard` for the chart, plus `helm` and
  `kubectl` pointed at the cluster.

**`deploy/values.yaml`.** Set the storage class and the AI endpoint/model; the
token is passed at install time, not committed.

```yaml
image:
  tag: main                    # or a release tag
mode: watch                    # continuous in-cluster worker (single writer)
persistence:
  storageClass: "<your-rwx-storage-class>"
  accessMode: ReadWriteMany
  size: 1Gi
ai:
  enabled: true
  endpoint: "http://<your-model-svc>.<ns>.svc.cluster.local:8000/v1/chat/completions"
  model: "<your-model-id>"
fetcher:
  buildsPerJob: 10
  workers: 4
  timeout: 120m                # per-pass cap; keep it > a cold serial pass on a slow model
  watchInterval: 5m
  reconcileInterval: 1h
```

> **Timeout note.** `fetcher.timeout` is a per-**pass** cap. On a slow
> self-hosted model, a cold pass analyzing many failures serially can be long;
> keep it comfortably larger than that pass or tail failures die with
> `context deadline exceeded`. The AI cache persists across passes, so only the
> cold pass is slow.

**Install.** From the engine checkout, pointing at your dashboard repo:

```bash
helm upgrade --install myproject deploy/helm/prow-ai-dashboard \
  --namespace myproject --create-namespace \
  -f ../myproject-dashboard/deploy/values.yaml \
  --set-file project.config=../myproject-dashboard/project.yaml \
  --set-file project.systemPrompt=../myproject-dashboard/prompts/system.md \
  --set ai.token=<token>   # any non-empty string if the endpoint needs no key
```

Provide the token via `--set ai.token=...` (or `ai.existingSecret` for
production, so it stays out of shell history and Helm metadata; see
[kubernetes.md](kubernetes.md)).

**Access and validate.** Port-forward the server:

```bash
kubectl -n myproject port-forward svc/myproject-prow-ai-dashboard-server 8080:80
open http://localhost:8080
```

Follow the worker as it populates data:

```bash
kubectl -n myproject logs -f deploy/myproject-prow-ai-dashboard-worker
```

The first cold pass can be slow on a self-hosted model; the dashboard fills in
once it completes. Then check:

- `http://localhost:8080/healthz` returns `ok`.
- `http://localhost:8080/api/capabilities` reports `"mode":"server"`.
- `http://localhost:8080/data/dashboard.json` lists your jobs (count matches
  Step 0), and its `generated_at` is recent.
- For a failing job, `.../data/jobs/<sanitized-job-id>.json` has failed
  `test_cases` with an `ai_summary` naming real symbols from your project.

**Optional: interactive actions.** To enable the admin-gated **File issue** /
**Propose fix** buttons (Kubernetes-native only), register a GitHub OAuth App and
set `server.actions` in `values.yaml`. See
[server.md](server.md#setting-up-oauth-mode) for the OAuth walkthrough and
[kubernetes.md](kubernetes.md#enabling-actions-with-helm) for the Helm wiring.

### Step 3b: GitHub Actions + Pages

Two thin callers of the engine's reusable workflows, both under
`.github/workflows/`. Set `project_dir` to match where `project.yaml` lives
(`.` for a dedicated repo, or your subdir such as `dashboard`). The deploy cron
(`*/30 * * * *` is common) is the only field most projects adjust.

**`deploy.yml`:**

```yaml
name: Deploy Dashboard

on:
  schedule:
    - cron: "*/30 * * * *"
  workflow_dispatch: {}
  push:
    branches: [main]

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: deploy
  cancel-in-progress: false

jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: "."        # or "dashboard" if you used a subdir
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

**`clear-cache.yml`** wipes the cache so the next deploy re-analyzes everything
from scratch. Optional, for an immediate full re-baseline; prompt edits already
take effect on the next deploy. Use the same `project_dir` as `deploy.yml`:

```yaml
name: Clear AI Cache

on:
  workflow_dispatch: {}

permissions:
  actions: write

jobs:
  clear:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-clear-cache.yml@main
    with:
      project_dir: "."        # or "dashboard" if you used a subdir
```

### Versioning and pinning

The `uses:` ref controls **both** the workflow and the engine code it builds
(checked out at the same commit, so they cannot drift).

```yaml
# Recommended: latest stable v1 (auto patch + minor, no breaking changes)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1

# Pin an exact release (fully frozen)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1.2.0

# Pre-release for testing
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1.0.0-rc.1

# Bleeding edge (no stability guarantee)
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
```

- Use the same ref on `reusable-clear-cache.yml`.
- `@vMAJOR` is the sweet spot: automatic fixes and features, with a deliberate
  bump only for a new major. See [releasing.md](releasing.md).
- Optional: set `min_engine_version` in `project.yaml` to warn (advisory only)
  when the pinned engine is older than your config expects.

### Pick a host repo (Pages)

Both options end up at `https://<org>.github.io/<repo>/`.

**Option A: dedicated repo.**

```bash
gh repo create my-org/<repo-name> --public
git clone https://github.com/my-org/<repo-name> && cd <repo-name>
# Copy the files into the root; use project_dir: "." in the workflows.
git add -A && git commit -m "Bootstrap prow-ai-dashboard" && git push -u origin main
```

**Option B: existing repo.** Add the configs to a subdirectory plus the two
workflows under `.github/workflows/`, and set `project_dir` to that subdir.

```bash
cd path/to/existing-repo
mkdir -p dashboard/prompts .github/workflows
# project.yaml -> dashboard/, system.md -> dashboard/prompts/,
# deploy.yml + clear-cache.yml -> .github/workflows/   (project_dir: "dashboard")
git add -A && git commit -m "Add prow-ai-dashboard" && git push
```

A repo can serve only one Pages site. If the existing repo already publishes
Pages, use Option A (a dedicated repo); enabling the dashboard's deploy would
replace the existing site. Non-Pages deploy targets are not yet supported.

### Manual GitHub config (Pages)

Done once by the host-repo owner; not scriptable from the engine.

```bash
# Enable Pages with the Actions build source
gh api repos/my-org/<repo-name>/pages -X POST -F build_type=workflow

# Set the AI token secret (gh prompts for the value)
gh secret set AI_TOKEN --repo my-org/<repo-name>
# Optional Slack notifications
gh secret set SLACK_WEBHOOK_URL --repo my-org/<repo-name>
```

### First deploy + validation (Pages)

```bash
gh workflow run deploy.yml --repo my-org/<repo-name>
gh run watch --repo my-org/<repo-name> --exit-status
```

After it goes green, check:

- `https://<org>.github.io/<repo>/` returns 200.
- `.../data/manifest.json` reflects your branding.
- `.../data/dashboard.json` lists your jobs (count matches Step 0).
- For a failing job, `.../data/jobs/<sanitized-job-id>.json` (the `job_id` from
  `dashboard.json`, with non-alphanumeric characters replaced) has failed
  `test_cases` with an `ai_summary` that names real symbols from your project.

If summaries read generically, add specifics to `prompts/system.md` and
redeploy. Prompt edits take effect automatically: the affected analyses re-run
on the next deploy with no cache clear. Two or three iterations is normal. (To
re-baseline everything at once, run the **Clear AI Cache** workflow first.)

## Optional (Pages): chat-completions endpoint unreachable from GitHub-hosted runners

On the Pages path, if your endpoint is private (a cloud private endpoint, a K8s
ClusterIP service, on-prem inference), GitHub-hosted runners cannot reach it.
Two options (or switch to the Kubernetes-native path in Step 3a, where the fetch
runs in-cluster next to the endpoint):

**Fetch locally, publish pre-fetched data.** Set `skip-fetch: true` under
`with:` in `deploy.yml` so each deploy publishes the committed
`<project_dir>/data/` instead of running the fetcher. Then run the fetcher where
the endpoint is reachable and commit the output:

```bash
cd /tmp/engine/backend && go build -o /tmp/fetcher ./cmd/fetcher
# <project_dir> is the repo root for a dedicated repo, or the subdir (e.g.
# dashboard) for an existing one; the deploy reads <project_dir>/data/.
AI_ENDPOINT="http://localhost:8000/v1/chat/completions" \
AI_TOKEN="<key or any non-empty string>" AI_MODEL="<model id>" \
  /tmp/fetcher -project-dir=<project_dir> -out=<project_dir>/data -ai

git add <project_dir>/data && git commit -m "Refresh prefetched data" && git push
```

**Self-hosted runner with cluster-internal access.** For automated runs when the
endpoint lives in a Kubernetes cluster, run the deploy on an in-cluster runner:

```yaml
uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1
with:
  project_dir: .
  runs-on: my-runner   # ARC runner-set name (a JSON array also works)
  ai-endpoint: http://your-svc.ns.svc.cluster.local:8000/v1/chat/completions
```

See [self-hosted-runner-in-cluster.md](self-hosted-runner-in-cluster.md) for the
full ARC install and tuning walkthrough.
