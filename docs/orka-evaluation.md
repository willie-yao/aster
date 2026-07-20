# Evaluating Orka safely

Use a separate Helm release and data claim when comparing Orka with an existing
dashboard. This keeps the current server and PVC available until the new output
has been reviewed.

## Safety controls

Set these values on the evaluation release:

```yaml
mode: cron
analysis: orka

fetcher:
  suspend: true

orka:
  sideEffects:
    enabled: false

server:
  actions:
    enabled: false
```

`fetcher.suspend` prevents scheduled CronJob starts. A Job created manually from
the CronJob still runs. `orka.sideEffects.enabled=false` keeps analysis and
job-level pattern finalization enabled, but skips email notifications, issue
reconciliation, and fix PR generation. It does not disable interactive server
actions, so disable those separately on an evaluation release.

## Choose the type of fresh run

| Run | Data PVC | `orka.version` | Reuses |
| --- | --- | --- | --- |
| Warm rerun | Existing | Unchanged | Dashboard cache and matching per-test and pattern Tasks |
| Fresh dashboard data | New empty PVC | Unchanged | Potentially matching pattern Tasks only |
| Fully cold Orka evaluation | New empty PVC | New value | Neither dashboard cache nor prior per-test or pattern results |

The producer stores a private result-validation key in `analysis-manifest.json`
on the data PVC and includes its hash in every per-test Task identity. A new PVC
generates a new key, so its per-test Tasks are cold even when `orka.version` is
unchanged. Pattern Task names do not include that key directly, so an unchanged
version can reuse an exact matching pattern prompt. Set a new `orka.version` to
force new pattern Tasks as well. Old Tasks remain available until removed
separately.

## Create a fresh claim

From a source checkout, the PVC helper can copy the storage shape of the current
claim without copying its data. It prints the manifest unless `--create` is
provided.

```bash
kubectl -n dashboards get pvc

deploy/helm/prow-ai-dashboard/create-fresh-pvc.sh --create \
  dashboards capz-prow-ai-dashboard-data capz-orka-eval-data
```

The helper never changes or deletes the source claim. Confirm that the new claim
is `Bound` before continuing. A storage class using `WaitForFirstConsumer` may
remain `Pending` until the first evaluation pod is scheduled.

Chart-managed claims carry `helm.sh/resource-policy: keep` by default through
`persistence.retain=true`. This preserves the old data if a release switches to
an external claim or is uninstalled. Retained claims must be deleted manually
after the rollback window.

## Install an isolated evaluation release

Use the same consumer config and tested image values as the primary release, but
choose a second release name and the fresh claim:

```bash
EVALUATION_ID=$(date -u +eval-%Y%m%d%H%M%S)

helm upgrade --install capz-orka-eval deploy/helm/prow-ai-dashboard \
  --namespace dashboards \
  -f <consumer-values.yaml> \
  --set mode=cron \
  --set analysis=orka \
  --set fetcher.suspend=true \
  --set orka.sideEffects.enabled=false \
  --set server.actions.enabled=false \
  --set persistence.enabled=false \
  --set persistence.existingClaim=capz-orka-eval-data \
  --set-string orka.version="$EVALUATION_ID"
```

A separate release gets its own CronJob, server, artifact Tool service, base Tool
ConfigMap, and RBAC names. Do not point it at the primary release's claim.

## Trigger and inspect one run

The trigger helper requires the CronJob to be suspended, checks for active Jobs,
and generates a unique Job name so repeated evaluations do not collide. The
check is not a distributed lock, so do not invoke the helper concurrently:

```bash
deploy/helm/prow-ai-dashboard/run-cronjob-now.sh --wait \
  dashboards capz-orka-eval-prow-ai-dashboard-fetcher
```

Without `--wait`, the helper prints commands for following logs and checking the
Job. The CronJob can remain suspended while manually created Jobs run.

Inspect the isolated server after the Job completes:

```bash
kubectl -n dashboards port-forward \
  svc/capz-orka-eval-prow-ai-dashboard-server 8081:80
open http://localhost:8081
```

Before cutover, verify that the Job completed, the dashboard contains the
expected jobs and analysis, no tests remain unexpectedly pending, and no email,
issue, or fix PR side effects occurred.

## Cut over and roll back

Re-run the primary release's normal Helm upgrade command and point it at the
validated claim:

```yaml
persistence:
  enabled: false
  existingClaim: capz-orka-eval-data
```

Keep the old claim. To roll back, run the same upgrade with its prior claim:

```yaml
persistence:
  enabled: false
  existingClaim: capz-prow-ai-dashboard-data
```

Only delete the evaluation release, old Tasks, or either PVC after the rollback
window has ended and the active claim has been confirmed. No helper in this
repository deletes Kubernetes or cloud resources.
