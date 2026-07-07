# Running the deploy on an in-cluster self-hosted runner

This applies to the **GitHub Actions + Pages** path. (If you instead run the
dashboard [Kubernetes-native](kubernetes.md), the fetch already runs in-cluster
next to your endpoint and none of this is needed.)

The deploy workflow runs the fetcher, which calls your chat-completions endpoint over HTTP. By
default it runs on a GitHub-hosted runner, which only has public internet
access. If your AI inference stack runs **inside a Kubernetes cluster** and is
exposed only as a `ClusterIP` Service (no public ingress), a GitHub-hosted
runner cannot reach it.

The clean fix is to run the deploy job on a **self-hosted runner that lives in
the same cluster**. The runner reaches your model over cluster DNS, so the
endpoint never has to be exposed publicly, and the dashboard refreshes on its
schedule with no machine of yours in the loop.

This is one of two escape hatches for a private endpoint on the Pages path; the
other is fetching locally and publishing pre-fetched data with `skip-fetch: true`
(see
[onboarding-a-new-project.md](onboarding-a-new-project.md#optional-pages-chat-completions-endpoint-unreachable-from-github-hosted-runners)).
Prefer the in-cluster runner for sustained, automated runs.

## How it works

```
GitHub (cron / push)
    -> dispatches the deploy job
        -> ephemeral runner pod starts in your cluster
            -> checks out the engine + your consumer repo, builds the fetcher
            -> fetcher calls your in-cluster AI Service over cluster DNS
            -> builds the frontend, deploys to GitHub Pages
        -> runner pod is deleted (ephemeral)
```

The runner pod needs:

- **Egress** to `github.com`, `raw.githubusercontent.com`, and
  `storage.googleapis.com` / `gcsweb.k8s.io` (job discovery + GCS artifacts).
- **In-cluster reach** to your model Service (cross-namespace `ClusterIP` is
  open unless a NetworkPolicy blocks it).

No GPU is required: the runner only makes HTTP calls and a Node build. A couple
of CPU cores and a few GB of memory is plenty.

## Prerequisites

- A Kubernetes cluster that hosts your chat-completions endpoint, with `kubectl` + `helm`
  access. Installing the runner controller creates cluster-scoped resources, so
  you need permission to install CRDs (cluster-admin, or a scoped install your
  platform team performs).
- The cluster has outbound internet access (see egress above). If it does not,
  this approach cannot work; use the local-fetch + `skip-fetch` path instead.

This guide uses [Actions Runner Controller
(ARC)](https://github.com/actions/actions-runner-controller) with the current
`gha-runner-scale-set` charts. Replace `my-org/my-consumer-repo` with your
dashboard repo throughout.

## Step 1: create a runner registration token

The runner authenticates to GitHub to receive jobs. A fine-grained PAT scoped
to the dashboard repo is the simplest option:

- GitHub -> Settings -> Developer settings -> Fine-grained tokens -> Generate.
- Repository access: only your dashboard repo.
- Repository permissions: **Administration: Read and write** (required to
  register self-hosted runners), Metadata: Read-only.

Copy the token. For an org-wide runner shared across several dashboards, use a
GitHub App instead (see the ARC docs); the rest of the setup is the same.

## Step 2: install the runner controller

```bash
helm install arc \
  --namespace arc-systems --create-namespace \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller

kubectl get pods -n arc-systems   # controller should be Running
```

## Step 3: store the token as a secret

```bash
kubectl create namespace arc-runners

read -rsp 'Paste PAT: ' GH_PAT; echo
kubectl create secret generic arc-gh-secret \
  -n arc-runners --from-literal=github_token="$GH_PAT"
unset GH_PAT
```

## Step 4: install the runner scale set

```bash
helm install my-runner \
  --namespace arc-runners \
  --set githubConfigUrl="https://github.com/my-org/my-consumer-repo" \
  --set githubConfigSecret=arc-gh-secret \
  --set minRunners=0 \
  --set maxRunners=2 \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set
```

Notes:

- The Helm **release name** (`my-runner`) becomes the runner set name and the
  `runs-on:` label your workflow targets. With current ARC you select a runner
  set by this single name, not by a `[self-hosted, ...]` array.
- `minRunners: 0` means no idle pods until a job arrives; `maxRunners` caps
  concurrency.
- The runner namespace must be able to reach your model Service. Cross-namespace
  `ClusterIP` traffic is open by default; a restrictive NetworkPolicy can block
  it (see Troubleshooting).

Verify it registered:

```bash
kubectl get pods -n arc-systems        # a listener pod appears for the set
# GitHub UI: repo -> Settings -> Actions -> Runners -> your set is listed
```

## Step 5: confirm the runner can reach your endpoint

Run a throwaway pod in the runner namespace and curl your model's
OpenAI-compatible endpoint:

```bash
kubectl run netcheck -n arc-runners --rm -i --restart=Never \
  --image=curlimages/curl --command -- \
  sh -c 'curl -sS -m 15 -o /dev/null -w "HTTP %{http_code}\n" \
    http://MY-MODEL-SERVICE.MY-NAMESPACE.svc:8000/v1/models'
```

A `200` confirms cluster DNS and reachability. If it hangs or fails, a
NetworkPolicy is likely blocking cross-namespace traffic.

## Step 6: point the deploy workflow at the runner

In your consumer repo's `.github/workflows/deploy.yml`, set `runs-on:` to the
runner set name and `ai-endpoint:` to the in-cluster Service URL:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    with:
      project_dir: "."
      runs-on: "my-runner"   # the ARC runner-set name from Step 4
      ai-endpoint: "http://MY-MODEL-SERVICE.MY-NAMESPACE.svc:8000/v1/chat/completions"
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
```

`runs-on:` accepts a bare label (the runner-set name) or a JSON-encoded array
for multi-label runners. The endpoint and model can also be supplied via repo
variables instead of the workflow file; see
[ai-providers.md](ai-providers.md). If your endpoint needs no auth, set
`AI_TOKEN` to any non-empty placeholder (the fetcher requires it to be set).

Commit and push. The next scheduled or pushed deploy runs on the in-cluster
runner and fetches against your endpoint directly.

## Tuning for an in-cluster model

A self-hosted model you run yourself behaves differently from a large hosted
provider, and a few `project.yaml` knobs matter more here:

- **`ai.concurrency`** parallelizes analyses. A dedicated endpoint can absorb
  some parallelism, but a single large model on limited GPUs saturates quickly:
  too high a value inflates per-call latency until analyses hit the per-failure
  timeout. Start at `2` and raise only if the endpoint keeps up.
- **`ai.timeout`** is the per-failure wall-clock budget (default `5m`). A slow
  model running a deep, many-iteration investigation can exceed it, and a
  timeout discards that analysis. Raise it (e.g. `15m`) for large local models.
- **`fetch-timeout`** (a workflow input, default `30m`) caps the whole fetch. A
  cold cache against a slow model can need longer; the cache is incremental, so
  analyses that do not finish in one run are retried on the next.

See [agentic.md](agentic.md#tuning-by-model-tier) for the full set of model-tier
knobs.

## Troubleshooting

- **Runner never appears in the GitHub UI.** The PAT lacks
  `Administration: write`, or `githubConfigUrl` is wrong. Check the controller
  logs: `kubectl logs -n arc-systems deploy/arc-gha-rs-controller`.
- **Jobs queue forever.** No runner is registered for that `runs-on` label;
  confirm the label matches the runner-set (release) name exactly.
- **`netcheck` (Step 5) fails.** A NetworkPolicy blocks `arc-runners` ->
  your model's namespace. Either run the scale set in the model's namespace, or
  add a NetworkPolicy that allows the egress.
- **Fetch fails to reach github.com or GCS.** The cluster has no outbound
  internet; this approach needs egress. Use the local-fetch + `skip-fetch` path
  instead.
- **Analyses error with "context deadline exceeded".** The model is too slow
  for the current settings under load; lower `ai.concurrency` and/or raise
  `ai.timeout` (see Tuning above).

## Teardown

```bash
helm uninstall my-runner -n arc-runners
helm uninstall arc -n arc-systems
kubectl delete namespace arc-runners arc-systems
```

Then revoke the PAT and, if you want to fall back to publishing pre-fetched
data, set `skip-fetch: true` in the deploy workflow.
