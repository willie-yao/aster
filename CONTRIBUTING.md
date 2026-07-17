# Contributing

See [Local development](docs/development.md) for setup and
[Testing](docs/testing.md) for the validation matrix.

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

Breaking changes to `project.yaml`, reusable workflow inputs, or published JSON
need an entry under `CHANGELOG.md` `[Unreleased]`. Pure documentation corrections
do not.
