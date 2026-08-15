# Migrating from prow-ai-dashboard

Aster `v0.9.0-rc.2` is the renamed engine release for existing
`prow-ai-dashboard` consumers. The migration changes published repository and
artifact coordinates. It does not require renaming consumer configuration,
persisted state, Helm releases, or compatibility identifiers.

## Choose one exact release

Use one exact version across the workflow, CLI, application image, and charts.
For the current release candidate, use the Git tag `v0.9.0-rc.2`, image tag
`v0.9.0-rc.2`, and chart version `0.9.0-rc.2`. After a stable release is
published, use its exact `vMAJOR.MINOR.PATCH` tag and matching chart version.
Do not mix release candidates, stable releases, moving refs, or `main`.

Before changing coordinates, record the current consumer commit, workflow ref,
Helm revisions, image digests, release names, namespaces, and persistent volume
claims. Back up the persistent data directory or volume with its metadata.

## Update published coordinates

| Interface | Previous coordinate | Aster coordinate |
| --- | --- | --- |
| Reusable Pages workflow | `willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@<exact-version>` | `willie-yao/aster/.github/workflows/reusable-deploy.yml@v0.9.0-rc.2` |
| Go CLI | `github.com/willie-yao/prow-ai-dashboard/backend/cmd/fetcher@<exact-version>` | `github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2` |
| CLI release asset | `prow-ai-dashboard-fetcher-<tag>-<target>` | `aster-<tag>-<target>` |
| Application image | `ghcr.io/willie-yao/prow-ai-dashboard:<tag>` | `ghcr.io/willie-yao/aster:<tag>` |
| Component images | `ghcr.io/willie-yao/prow-ai-dashboard/<component>:<tag>` | `ghcr.io/willie-yao/aster/<component>:<tag>` |
| Application chart | `oci://ghcr.io/willie-yao/charts/prow-ai-dashboard` | `oci://ghcr.io/willie-yao/charts/aster` |
| Platform chart | `oci://ghcr.io/willie-yao/charts/prow-ai-dashboard-platform` | `oci://ghcr.io/willie-yao/charts/aster-platform` |

Update only component images enabled in your reviewed values. Keep immutable
digest pins where the feature requires them. The exact CLI download and chart
verification procedure is in the [Kubernetes quickstart](kubernetes.md).

## Preserve compatibility contracts

Keep the existing consumer files and operational identities:

- Preserve `project.yaml`, `prompts/system.md`, consumer skills, and deployment
  values. Do not regenerate them just to adopt the Aster name.
- Preserve the entire cache and state directory or PVC, including files such as
  `ai_cache.json`, `ai_traces.json`, `issue_state.json`,
  `notification_state.json`, `remediation_state.json`,
  `action_request_state.json`, and action audit state.
- Preserve `PROW_AI_*` environment variables and `prow-ai-dashboard/*`
  Kubernetes labels and annotations.
- Preserve issue markers such as `<!-- prow-ai-dashboard-key:... -->` and Fix PR
  markers such as `<!-- prow-ai-dashboard-fix:... -->`. Editing them can defeat
  adoption and create duplicate GitHub writes.
- Preserve Helm release names, namespaces, PVC names, ConfigMap identities, and
  other platform resource names unless a separately reviewed infrastructure
  migration says otherwise.

These identifiers are intentional compatibility contracts, not unfinished
branding work.

The platform chart still renders `prow-ai-dashboard-platform` in selected
resource names and ownership labels. That identity predates Aster and is used by
existing Helm releases, live-doctor selectors, platform binding ConfigMaps, and
ownership markers. Renaming it in place could orphan resources or make rollback
and ownership detection unsafe. Any future rename needs an explicit migration,
adoption, collision, and rollback design. Do not override the name merely for
cosmetic consistency.

## Validate before changing a deployment

Run the released CLI's read-only consumer doctor:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.2 \
  onboard doctor \
  -project-dir ./my-dashboard
```

For Pages, change only the reusable workflow repository and exact ref first.
Review the workflow diff, rerun doctor, and trigger one deployment. Confirm
`data/provenance.json`, the expected jobs, and reuse of existing analysis data.

For Kubernetes, download the exact `aster` CLI as described in the
[Kubernetes quickstart](kubernetes.md), update chart and image repositories,
and retain the release and persistence identities. Run the live doctor and a
local dry-run before upgrading:

```bash
"$ASTER" kubernetes doctor \
  --action upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version 0.9.0-rc.2

"$ASTER" kubernetes upgrade \
  --project-dir "$PROJECT_DIR" \
  --values deploy/values.yaml \
  --release "$RELEASE" \
  --namespace "$NAMESPACE" \
  --kube-context "$CONTEXT" \
  --chart-version 0.9.0-rc.2 \
  --dry-run
```

Review the rendered plan, then repeat the upgrade without `--dry-run`. Verify
the existing PVC is mounted, the expected job is present, private state remains
unserved, and enabled authenticated features still work.

## Roll back

Pages rollback restores the prior consumer commit or workflow repository and
exact ref, then reruns the workflow. Kubernetes rollback restores the prior
Helm revision and the matching consumer commit, CLI, chart, and image versions.
Do not delete or rename the persistent volume, release, compatibility labels,
binding ConfigMaps, state files, or GitHub markers during rollback.

Follow the full [Kubernetes rollback procedure](kubernetes.md#roll-back) when
applicable. If a migration check fails, stop before changing additional
coordinates and return all surfaces to the last known-good exact version.
