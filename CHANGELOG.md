# Changelog

All notable changes to the Aster engine are documented here. The engine follows [Semantic Versioning](https://semver.org): consumers pin it via `uses: willie-yao/aster/.github/workflows/reusable-deploy.yml@<ref>`, and the pinned ref controls both the workflow and the engine code it builds.

What bumps what:

- **MAJOR**: removing or renaming a `project.yaml` field, changing a reusable workflow input contract, or breaking the published data JSON schema.
- **MINOR**: a new optional config field, tool, or feature with safe defaults. Internal cache-version bumps (which force re-analysis on upgrade) are at least minor.
- **PATCH**: bug fixes, prompt tweaks, performance.

See [the release guide](docs/releasing.md#versioning) for the release process and how to pin a consumer to a reviewed version.

## Releases

Each release has its own notes file, named for its tag. See [the release guide](docs/releasing.md#release-notes) for how those notes are assembled.

- [v0.9.0-rc.13](changelog/v0.9.0-rc.13.md) - 2026-09-02
- [v0.9.0-rc.12](changelog/v0.9.0-rc.12.md) - 2026-08-27
- [v0.9.0-rc.11](changelog/v0.9.0-rc.11.md) - 2026-08-27
- [v0.9.0-rc.10](changelog/v0.9.0-rc.10.md) - 2026-08-26
- [v0.9.0-rc.9](changelog/v0.9.0-rc.9.md) - 2026-08-20
- [v0.9.0-rc.8](changelog/v0.9.0-rc.8.md) - 2026-08-19
- [v0.9.0-rc.7](changelog/v0.9.0-rc.7.md) - 2026-08-19
- [v0.9.0-rc.6](changelog/v0.9.0-rc.6.md) - 2026-08-19
- [v0.9.0-rc.5](changelog/v0.9.0-rc.5.md) - 2026-08-18
- [v0.9.0-rc.4](changelog/v0.9.0-rc.4.md) - 2026-08-18
- [v0.9.0-rc.3](changelog/v0.9.0-rc.3.md) - 2026-08-17
- [v0.9.0-rc.2](changelog/v0.9.0-rc.2.md) - 2026-08-15

Release notes carried over from `prow-ai-dashboard`, the repository Aster was migrated from, are archived in [changelog/legacy.md](changelog/legacy.md). The migration restarted the version line at `v0.9.0`, and none of those tags exist in this repository.
