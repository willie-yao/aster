# Onboarding a project

This quickstart creates the small consumer repository that points to the shared
Aster engine. For every flag, update rule, plan artifact, and automation
contract, use the [onboarding reference](onboarding-reference.md).

## Choose an onboarding method

All methods create the same consumer contract and can target GitHub Pages or
Kubernetes.

| Method | Use it when | Start |
| --- | --- | --- |
| Interactive wizard | You want guided discovery and review. | [Interactive wizard](#interactive-wizard) |
| Coding agent-assisted | You want an agent to run the same reviewed CLI plan and handoff. | [Coding agent-assisted onboarding](#coding-agent-assisted-onboarding) |
| Non-interactive CLI | Inputs are known and the process must be repeatable. | [Non-interactive CLI onboarding](#non-interactive-cli-onboarding) |
| Manual setup | You need to author every consumer file directly. | [Manual setup](#manual-setup) |

## What onboarding creates

The common files are:

```text
project.yaml
prompts/system.md
.github/workflows/deploy.yml   # GitHub Pages
# or
deploy/values.yaml             # Kubernetes
```

Pages scaffolds also include `CHECKLIST.md`. Kubernetes scaffolds include
`deploy/README.md`. Handoff prompt mode adds reviewable instructions for
completing `prompts/system.md`. The consumer owns these files; engine code stays
in the Aster repository.

## Interactive wizard

From the source repository whose jobs you want to monitor, run the current
release exactly:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.3 onboard \
  -engine-ref v0.9.0-rc.3
```

The wizard discovers matching Prow and TestGrid jobs, asks for the deployment
and AI coordinates, renders every file in memory, validates the result, and
shows the complete plan. The final confirmation defaults to no. Cancellation
leaves the filesystem unchanged.

`-engine-ref` pins a generated Pages workflow to the same exact release. It does
not select Kubernetes chart or image versions. For a private GitHub repository,
export `GITHUB_TOKEN` for API reads. The token is not written to the scaffold.

## Coding agent-assisted onboarding

Install the portable setup and diagnostic-authoring skills:

```bash
npx --yes skills@latest add willie-yao/aster \
  --skill setup-aster-consumer author-aster-diagnostics \
  --agent codex \
  --global \
  --yes
```

Then make one concrete request:

```text
Use $setup-aster-consumer to create a Pages consumer for
https://github.com/kubernetes-sigs/kueue.
```

The setup skill must use `aster onboard`, not hand-write the scaffold. It runs
read-only discovery, prepares an exact dry-run plan, preserves consumer-owned
prompt and skill files during updates, leaves source-only template placeholders
for review, waits for confirmation before applying the reviewed plan, and
validates the resulting consumer. Repository creation, pushes, pull requests,
GitHub settings, Secret writes, and cluster writes remain separate confirmation-gated actions.

After setup, use `$author-aster-diagnostics` to evaluate representative
historical failures, improve `prompts/system.md`, and place any proposed recipes
under `proposals/skills/`. The authoring workflow does not activate recipes.

Update installed global skills with:

```bash
npx --yes skills@latest update \
  setup-aster-consumer author-aster-diagnostics \
  --global \
  --yes
```

Review skill changes before using them in an automated or write-enabled flow.

## Non-interactive CLI onboarding

Use the flagged CLI when all required inputs are known. This example creates a
Pages consumer:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.3 onboard \
  -engine-ref v0.9.0-rc.3 \
  -non-interactive \
  -testgrid "<testgrid-dashboard>" \
  -dashboard-repo "<owner>/<dashboard-repo>" \
  -source-repo "<owner>/<source-repo>" \
  -artifact-access public \
  -deployment-reason "Artifacts and provider are reachable from GitHub Actions." \
  -out ./my-dashboard
```

For durable automation, use `-dry-run -plan-out`, review the digest, then apply
that exact artifact with `-apply-plan` and `-plan-digest`. The
[onboarding reference](onboarding-reference.md#dry-run-and-reviewed-plan-application)
contains the complete contract.

## Review the generated files

Before deployment, verify:

1. `project.yaml` selects the expected jobs, storage, branding, source repository,
   and analysis policy.
2. `prompts/system.md` contains only grounded project knowledge and retains the
   required runbook headings.
3. The Pages workflow or Kubernetes values use the intended deployment path.
4. Generated checklist items and placeholders have been resolved.
5. No credential appears in a committed file.

Use [Project configuration](project-configuration.md) for exact fields and
[Writing the project prompt](writing-prompts.md) for prompt ownership.

## Choose Pages or Kubernetes

| Deployment | Choose it when | Guide |
| --- | --- | --- |
| GitHub Pages | The dashboard may be public and read-only, artifacts are public, and the model endpoint is reachable from GitHub Actions. | [GitHub Actions and Pages](github-pages.md) |
| Kubernetes | Artifacts or the provider are private to the cluster, state needs shared persistence, or authenticated server features are required. | [Kubernetes quickstart](kubernetes.md) |

Standard onboarding configures authoritative in-process analysis. Fix PRs and
Agent Sandbox shadows are separate opt-in features and are not required for a
working dashboard.

## Manual setup

Create the same consumer files directly when the generator is not appropriate.
Start from:

- [`configs/example/project.yaml`](../configs/example/project.yaml)
- [`configs/example/prompts/system.md`](../configs/example/prompts/system.md)
- [Project configuration](project-configuration.md)
- [GitHub Actions and Pages](github-pages.md) or
  [Kubernetes quickstart](kubernetes.md)

Do not copy engine code into the consumer repository.

## Validate with `onboard doctor`

Run the read-only validator after generation and after meaningful edits:

```bash
go run github.com/willie-yao/aster/backend/cmd/aster@v0.9.0-rc.3 \
  onboard doctor \
  -project-dir ./my-dashboard
```

Doctor validates the project and prompt, deployment coordinates, and a real
nonzero Prow discovery sweep. It does not call the model provider or inspect a
Kubernetes cluster.

## Deploy

Follow the generated `CHECKLIST.md` for Pages or `deploy/README.md` for
Kubernetes. The canonical deployment guides are:

- [GitHub Actions and Pages](github-pages.md)
- [Kubernetes quickstart](kubernetes.md)
- [Flux GitOps deployment](kubernetes-gitops.md)
- [Kubernetes platform setup](kubernetes-platform.md)

Deploy the smallest working read-only configuration first. Add chat,
notifications, actions, Fix PRs, or maintainer shadows only after jobs and
analysis are healthy. The [documentation index](README.md) owns the feature map
and recommended enablement order.
