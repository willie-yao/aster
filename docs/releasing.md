# Releasing the engine

How to cut a release of the Aster engine. Consumers on the GitHub
Actions + Pages path pin the engine through the reusable deploy workflow, so for
them a "release" is just a git tag plus a GitHub Release. A tag also publishes
the Kubernetes-native artifacts: the container image and the Helm chart.

## Versioning

[Semantic Versioning](https://semver.org), tags prefixed with `v`:

- `vMAJOR.MINOR.PATCH` for stable releases (e.g. `v1.2.0`).
- `vMAJOR.MINOR.PATCH-beta.N` / `-rc.N` for pre-releases (e.g. `v1.0.0-beta.1`).
- A moving `vMAJOR` alias (e.g. `v1`) tracks the latest stable release in that
  major, created/advanced automatically on each stable release.

Before the first stable release in a major, the moving alias does not exist.
Consumers must use `@main`, a commit SHA, or an exact prerelease tag that is
already published. Do not document `@v1` as usable until `v1.0.0` exists.

See [CHANGELOG.md](../CHANGELOG.md) for what bumps major/minor/patch. Note that
internal critique-version bumps and stronger investigation floors can force
re-analysis on upgrade and are therefore at least a minor bump; call them out in
the changelog.

## Cutting a release

1. Make sure `main` is green and the `## [Unreleased]` section of
   `CHANGELOG.md` is up to date. Rename it to the version being released and add
   a fresh `## [Unreleased]` above it.
2. Tag and push:
   ```bash
   git checkout main && git pull
   git tag v1.0.0-beta.1
   git push origin v1.0.0-beta.1
   ```
3. The `Release` workflow (`.github/workflows/release.yml`) runs on the tag:
   - re-runs the full CI gate against the tagged commit,
   - creates the GitHub Release with auto-generated notes (marked
     **pre-release** when the tag has a `-beta`/`-rc` suffix),
   - packages the application and platform Helm charts at the release version,
     pushes them to `oci://ghcr.io/<owner>/charts/aster` and
     `oci://ghcr.io/<owner>/charts/aster-platform`, and attaches
     `aster-<version>.tgz` and `aster-platform-<version>.tgz` to the release,
   - cross-compiles the `aster` CLI for Linux and macOS on amd64 and arm64,
     attaches `aster-<tag>-<target>` for each target plus `SHA256SUMS`,
   - waits for the matching engine, remote-fixer, and Agent Sandbox Fix executor
     images and verifies their embedded source revision before publishing charts,
     the GitHub Release, or the stable major alias,
   - for a **stable** tag only, fast-forwards the `vMAJOR` alias after both
     charts are packaged, pushed, and attached successfully.

   In parallel, `.github/workflows/image.yml` builds and pushes the engine,
   analyzer, local fixer, remote fixer, and Agent Sandbox executor images with `main`,
   `sha-<short-commit>`, and applicable semantic-version tags. The Agent Sandbox
   executor is published for `linux/amd64` at
   `ghcr.io/<owner>/aster/agent-sandbox-fix-executor`. Tags are
   discovery aliases only; deployed Agent Sandbox configuration requires the
   resolved OCI digest.
   The git-only remote fixer is published at
   `ghcr.io/<owner>/aster/remote-fixer` for dashboard-side patch
   reconstruction and contains neither OpenCode nor srt.

## Pre-release to stable

Iterate pre-releases until the release is solid, then cut the stable tag:

```
v1.0.0-beta.1  ->  v1.0.0-beta.2  ->  v1.0.0-rc.1  ->  v1.0.0
```

Pre-releases never move the `vMAJOR` alias and are never marked "latest", so a
consumer on `@v1` is unaffected until `v1.0.0` ships. Test a pre-release by
pinning a consumer to the exact tag (e.g. `@v1.0.0-beta.1`).

## Release branches (backports)

While everything ships from `main`, no release branch is needed. Create one only
when you must patch an older major after `main` has moved on:

1. At a `vMAJOR.0.0` stable release, cut `release-MAJOR.x` from the tag
   (e.g. `release-1.x` from `v1.0.0`).
2. Backport a fix: land it on `main`, then cherry-pick to the release branch.
3. Tag the next patch/minor from the branch (e.g. `v1.4.1`); the release
   workflow advances the `v1` alias.

Do not pre-create empty release branches; create `release-N.x` only when there
is a real backport to make.

## Rolling back

A bad release: cut a new patch with the fix. To stop consumers pulling a broken
stable, point the `vMAJOR` alias back at the last good tag:

```bash
git tag -f v1 v1.3.4        # last known-good
git push origin -f refs/tags/v1
```

Consumers pinned to an exact tag are unaffected.

## Causal critic executor image

The `agent-sandbox-critic-executor` Docker target is currently CI-buildable but
is intentionally absent from the publishing workflow. Do not add it to
`.github/workflows/image.yml` until cold critic evaluation, secure gateway
identity validation, and an explicit publication decision are complete.
