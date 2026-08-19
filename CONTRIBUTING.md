# Contributing

See [Local development](docs/development.md) for setup and
[Testing](docs/testing.md) for the validation matrix.

## Finding your way around

`backend/internal` has many small packages. Start with the "Repo layout" and
"How the pieces fit" sections in [AGENTS.md](AGENTS.md): the layout map is
grouped by role and covers every package, and the second section names the five
packages most changes touch. Keep the map current when you add or remove a
package; `make check-repo-map` enforces it and runs in CI.

## Prerequisites

- Go 1.25 as declared by `backend/go.mod`
- Node.js 20 or newer
- npm
- `staticcheck` for the full backend validation
- Docker and Helm only for container or Kubernetes changes

## Workflow

1. Create a focused branch from `main`.
2. Follow the existing package and component patterns.
3. Keep comments short and factual.
4. Add or update tests with behavior changes.
5. Run the focused checks while iterating, then the full validation before a PR.

Do not add project-specific consumer configuration to this engine repository.
`configs/example` is documentation and smoke-test data only.

## Commit and PR style

Use a conventional, single-line commit subject. Put the detailed rationale and
verification commands in the pull request description.

Every pull request needs a `release-note` block in its description. The block is
published verbatim in the release notes, so write it for someone upgrading
Aster: what changed, and what it means for them. Multiple paragraphs are fine
and often right. Call out breaking changes to `project.yaml`, reusable workflow
inputs, or the published JSON explicitly.

    ```release-note
    Fix generation resolves its base from the failure's own branch, so a failure
    on a release branch no longer opens a pull request against the default one.
    ```

Write `NONE` when the change has no effect a user would notice: tests,
refactors, repo tooling, or documentation corrections. CI rejects a pull request
whose description carries no block.
