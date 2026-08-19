# Releasing the engine

How to cut a release of the Aster engine. Consumers on the GitHub
Actions + Pages path pin the engine through the reusable deploy workflow, so for
them a "release" is just a git tag plus a GitHub Release. A tag also publishes
the Kubernetes-native artifacts: the container image and the Helm chart.

## Versioning

[Semantic Versioning](https://semver.org), tags prefixed with `v`:

- `vMAJOR.MINOR.PATCH` for stable releases (e.g. `v1.2.0`).
- `vMAJOR.MINOR.PATCH-beta.N` / `-rc.N` for pre-releases (e.g. `v1.0.0-beta.1`).
- `backend/vMAJOR.MINOR.PATCH[-PRERELEASE]` pairs every root release tag with
  the same exact commit so the nested Go module resolves at that version.
- A moving `vMAJOR` alias (e.g. `v1`) tracks the latest stable release in that
  major, created/advanced automatically on each stable release.

Before the first stable release in a major, the moving alias does not exist.
Consumers must use `@main`, a commit SHA, or an exact prerelease tag that is
already published. Do not document `@v1` as usable until `v1.0.0` exists.

See [CHANGELOG.md](../CHANGELOG.md) for what bumps major/minor/patch. Note that
internal critique-version bumps and stronger investigation floors can force
re-analysis on upgrade and are therefore at least a minor bump; call them out in
the release notes.

## Release notes

Each release has one notes file named for its tag, `changelog/<tag>.md`, listed
in the `CHANGELOG.md` index. The file holds the notes body alone, without a
version heading of its own.

Notes are assembled from the `release-note` blocks of the pull requests merged
since the previous tag. A prerelease covers everything since the previous tag; a
stable release covers everything since the previous *stable* tag, so it tells the
whole story of the versions that led up to it.

## Cutting a release

1. Make sure `main` is green. Write `changelog/<tag>.md` from the `release-note`
   blocks merged since the previous tag, and add the release to the index in
   `CHANGELOG.md`.
2. Create the root and nested-module tags at the same reviewed commit, then
   push both without force:
   ```bash
   git checkout main && git pull
   git tag v1.0.0-beta.1
   git tag backend/v1.0.0-beta.1
   git push origin v1.0.0-beta.1 backend/v1.0.0-beta.1
   ```
3. The `Release` workflow (`.github/workflows/release.yml`) runs on the tag:
   - re-runs the full CI gate against the tagged commit,
   - verifies both release tags identify the reviewed commit; if the root tag
     exists and only the module tag is missing, it creates the module tag with
     a non-force push before publishing,
   - creates the GitHub Release with auto-generated notes (marked
     **pre-release** when the tag has a `-beta`/`-rc` suffix),
   - packages the application and platform Helm charts at the release version,
     pushes them to `oci://ghcr.io/<owner>/charts/aster` and
     `oci://ghcr.io/<owner>/charts/aster-platform`, and attaches
     `aster-<version>.tgz` and `aster-platform-<version>.tgz` to the release,
   - cross-compiles the `aster` CLI for Linux and macOS on amd64 and arm64,
     attaches `aster-<tag>-<target>` for each target, an exact source archive,
     a machine-readable release manifest, and `SHA256SUMS`,
   - waits for the matching engine, remote-fixer, and Agent Sandbox Fix executor
     images and verifies their embedded source revision before publishing charts,
     the GitHub Release, or the stable major alias,
   - for a **stable** tag only, fast-forwards the `vMAJOR` alias after both
     charts are packaged, pushed, and attached successfully.

   In parallel, `.github/workflows/image.yml` publishes only the exact release
   tag for the application, remote fixer, and Agent Sandbox Fix executor. The
   analysis executor and stager are manual-only. The Fix
   executor is published for `linux/amd64` at
   `ghcr.io/<owner>/aster/agent-sandbox-fix-executor`; deployed Agent Sandbox
   configuration still requires the resolved OCI digest.
   The git-only remote fixer is published at
   `ghcr.io/<owner>/aster/remote-fixer` for dashboard-side patch
   reconstruction and contains neither OpenCode nor model credentials.

The `backend/` tag does not match the release or image workflow triggers, so it
does not publish a second GitHub Release or duplicate OCI artifacts. To inspect
tag state without changing it, run the publishing script with
`RELEASE_DRY_RUN=true`. To recover only a missing module tag without publishing
artifacts, use `RELEASE_TAGS_ONLY=true`. Both modes still reject invalid
versions, moved tags, and mismatched tag pairs.

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

## Building images from a branch

To deploy a commit without cutting a release, run the `Image` workflow manually
(Actions -> Image -> Run workflow) against the branch you want. It publishes the
application, remote fixer, and Agent Sandbox Fix executor at
`sha-<short>` for that commit, leaving release tags and the `vMAJOR` alias
untouched.

```bash
gh workflow run image.yml --ref main
```

Use the resulting `sha-<short>` tag to pin a deployment for testing. The Agent
Sandbox Fix executor is still pinned by digest, so resolve it after the run:

```bash
docker buildx imagetools inspect \
  ghcr.io/<owner>/aster/agent-sandbox-fix-executor:sha-<short> \
  --format '{{.Manifest.Digest}}'
```

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
