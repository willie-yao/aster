# Changelog

All notable changes to the Aster engine are documented here. The engine follows [Semantic Versioning](https://semver.org): consumers pin it via `uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@<ref>`, and the pinned ref controls both the workflow and the engine code it builds.

What bumps what:

- **MAJOR**: removing or renaming a `project.yaml` field, changing a reusable workflow input contract, or breaking the published data JSON schema.
- **MINOR**: a new optional config field, tool, or feature with safe defaults. Internal cache-version bumps (which force re-analysis on upgrade) are at least minor.
- **PATCH**: bug fixes, prompt tweaks, performance.

See [the release guide](docs/releasing.md#versioning) for the release process and how to pin a consumer to a reviewed version.

## Releases

Each release has its own notes file, named for its tag. See [the release guide](docs/releasing.md#release-notes) for how those notes are assembled.

- [v0.10.0-rc.2](changelog/v0.10.0-rc.2.md) - 2026-09-10
- [v0.10.0-rc.1](changelog/v0.10.0-rc.1.md) - 2026-09-10

Release notes carried over from `prow-ai-dashboard`, the repository Aster was migrated from, are archived in [changelog/legacy.md](changelog/legacy.md). None of those tags exist in this repository.
