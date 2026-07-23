# Releasing the engine

How to cut a release of the prow-ai-dashboard engine. Consumers on the GitHub
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
internal cache-version bumps (critique, skills, depth) force re-analysis on
upgrade and are therefore at least a minor bump; call them out in the changelog.

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
   - packages the Helm chart at the release version (with its image pinned to
     the tag), pushes it to `oci://ghcr.io/<owner>/charts/prow-ai-dashboard`,
     and attaches the `.tgz` to the release,
   - for a **stable** tag only, fast-forwards the `vMAJOR` alias to the tag.

   In parallel, `.github/workflows/image.yml` builds and pushes the container
   image (fetcher + server + SPA) to `ghcr.io/<owner>/prow-ai-dashboard` with
   the version tags.

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
