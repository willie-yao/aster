# Consumer setup decisions

## Input resolution

Do not ask again for a repository, URL, job, mode, path, artifact policy, or update policy already present in the request. Normalize explicit GitHub URLs, derive the project slug from the source name, and run read-only discovery before asking for a selector or consumer identity. A discovery suggestion may seed the local plan but does not authorize remote repository creation.

When invoked from the `Aster` engine checkout, never treat the engine's own Git `origin` as the source project unless explicitly named. If the source is absent, ask only that blocking question.

## Placement

| Choice | Use when | `-out` |
| --- | --- | --- |
| Current directory | The current repository owns the consumer files | `.` |
| Existing checkout | A consumer repository already exists locally | Its checkout path |
| Subdirectory | A monorepo owns a dedicated consumer directory | The subdirectory |
| Separate directory | Source and consumer stay independent | A sibling or explicit path |

Keep the plan, snapshots, logs, apply result, and setup handoff outside the consumer. Existing symlink ancestors are resolved before the plan is saved.

## Deployment

| Mode | Use when | Generated deployment files |
| --- | --- | --- |
| `pages` | Public or runner-reachable artifacts and provider, read-only dashboard, no persistent authenticated admin actions | `.github/workflows/deploy.yml`, `CHECKLIST.md` |
| `k8s` | Private or authenticated artifacts, cluster-local provider or endpoint, persistent state, or authenticated admin actions | `deploy/values.yaml`, `deploy/README.md` |

Record `-artifact-access` plus one or more `-deployment-reason` values. Consider artifact privacy, provider reachability from GitHub Actions, authentication, persistent state, admin actions, and cluster-local endpoints. Do not select Kubernetes merely because the source project uses Kubernetes. Record unknowns instead of inferring them.

For non-interactive Kubernetes setup, require exactly one reviewed ReadWriteMany storage coordinate: `-k8s-storage-class` for chart-provisioned storage or `-k8s-existing-claim` for a pre-provisioned PVC. Never infer it from provider or cluster type, and stop if the selected CLI does not advertise both flags.

## Discovery

Use `-testgrid` for Kubernetes TestGrid dashboards. Use `-bucket` for an artifact bucket not selected through TestGrid. Add `-gcsweb-base` only for a gcsweb gateway.

When exact jobs are named, prefer `-bucket` plus repeated `-exact-job` flags. The names are a hard boundary. A shared TestGrid dashboard containing additional jobs is unresolved unless the user accepts the wider scope. Presubmit tabs are not ingested unless `-include-presubmits` is explicitly selected.

The plan records the exact selected job identities, discovery digest, and pinned catalog revision when available. A missing catalog revision remains an explicit warning.

## Prompt ownership and updates

The setup-generated prompt is a source-only baseline. It has not been validated against historical failures. `$author-aster-diagnostics` owns that validation and any evidence-based prompt revision.

For an existing consumer:

1. Snapshot and hash `prompts/system.md` and `skills/*.yaml` or `skills/*.yml`.
2. Run without `-update-existing` to identify conflicts.
3. After approval, create a new plan with `-update-existing`.
4. Confirm that the existing prompt and every skill file are marked `preserve`.

`-update-existing` replaces only engine-generated files. The plan retains a separate source-only candidate and records both prompt hashes. This preserves stable cross-version knowledge by default.

Use `-replace-consumer-owned` only with `-update-existing`, only after reviewing the existing-versus-candidate diff, and only after explicit approval naming `prompts/system.md`. Existing skill files are never replaced or activated by setup.

## Artifact usability

The apply phase may sample up to five recent builds per selected job. It checks only whether recent builds and the expected artifact classes are readable: `prowjob.json`, `started.json`, `build-log.txt`, JUnit, and `artifacts/`. If all sampled builds lack JUnit, the handoff warns that test-level granularity may be unavailable and synthesized build-level failures may be required. The smoke check does not read a historical failure corpus, assign ownership, classify transience, or propose a fix.

## Reproducibility and handoff

A reproducible setup records:

- Engine path, resolved version, revision, and modified state.
- Source ref and revision.
- First-class test-infra repository, revision, and selected config files for TestGrid discovery.
- First-class artifact provider, bucket, and optional gateway or local base.
- Discovery selector, digest, and exact jobs.
- Deployment mode, reasons, and artifact access.
- Reviewed plan digest.
- Post-apply file modes, hashes, and create, replace, or preserve status.
- Doctor and artifact-smoke results.

Do not describe `@latest` as reproducible without the resolved version and revision. Validate `manifest/setup-handoff.json` with the bundled validator before sending it to `$author-aster-diagnostics`.

## Write boundaries

A request to set up the local consumer authorizes applying the exact reviewed plan after confirmation. It does not authorize rerunning discovery and writing a newly rebuilt plan. It also does not authorize Git initialization, remote repository creation, pushes, pull requests, Pages configuration, Secret writes, Helm installation, deployment, or recipe activation.
