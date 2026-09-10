# Releasing the engine

How to cut a release of the Aster engine. Consumers on the GitHub Actions + Pages path pin the engine through the reusable deploy workflow, so for them a "release" is just a git tag plus a GitHub Release. A tag also publishes the Kubernetes-native artifacts: the container image and the Helm chart.

## Versioning

[Semantic Versioning](https://semver.org), tags prefixed with `v`:

- `vMAJOR.MINOR.PATCH` for stable releases (e.g. `v1.2.0`).
- `vMAJOR.MINOR.PATCH-beta.N` / `-rc.N` for pre-releases (e.g. `v1.0.0-beta.1`).
- `backend/vMAJOR.MINOR.PATCH[-PRERELEASE]` pairs every root release tag with the same exact commit so the nested Go module resolves at that version.
- A moving `vMAJOR` alias (e.g. `v1`) tracks the latest stable release in that major, created/advanced automatically on each stable release.

Before the first stable release in a major, the moving alias does not exist. Consumers must use `@main`, a commit SHA, or an exact prerelease tag that is already published. Do not document `@v1` as usable until `v1.0.0` exists.

See [CHANGELOG.md](../CHANGELOG.md) for what bumps major/minor/patch. Note that internal critique-version bumps and stronger investigation floors can force re-analysis on upgrade and are therefore at least a minor bump; call them out in the release notes.

## Release notes

Each release has one notes file named for its tag, `changelog/<tag>.md`, listed in the `CHANGELOG.md` index. The file holds the notes body alone, without a version heading of its own. It is published verbatim as the GitHub Release body, so the release page shows the curated notes rather than a generated commit list. The publisher refuses to release a tag whose notes file is missing, empty, or absent from the index.

Notes are assembled from the `release-note` blocks of the pull requests merged since the previous tag. A prerelease covers everything since the previous tag; a stable release covers everything since the previous *stable* tag, so it tells the whole story of the versions that led up to it.

Because the notes are published verbatim as the release body, relative paths in them do not resolve against the repository. Link repository files with absolute URLs pinned to the tag, such as `https://github.com/willie-yao/aster/blob/<tag>/docs/<file>.md`. Rewrite any relative or root-relative link copied out of a `release-note` block.

## Cutting a release

1. Make sure `main` is green. Write `changelog/<tag>.md` from the `release-note` blocks merged since the previous tag, and add the release to the index in `CHANGELOG.md`. Keep `docs/supported-onboarding-release.txt`, current onboarding examples, and the setup skill pinned to the last published release.
2. Create the root and nested-module tags at the same reviewed commit, then push both without force:
   ```bash
   git checkout main && git pull
   git tag v1.0.0-beta.1
   git tag backend/v1.0.0-beta.1
   git push origin v1.0.0-beta.1 backend/v1.0.0-beta.1
   ```
3. The `Release` workflow (`.github/workflows/release.yml`) runs on the tag:
   - re-runs the full CI gate against the tagged commit,
   - verifies both release tags identify the reviewed commit; if the root tag exists and only the module tag is missing, it creates the module tag with a non-force push before publishing,
   - refuses to publish a tag that is not the newest version in the repository, comparing by semantic precedence rather than string order, so a prerelease sorts below the release it leads to and `beta` sorts below `rc`,
   - creates the GitHub Release from `changelog/<tag>.md` (marked **pre-release** when the tag has a `-beta`/`-rc` suffix),
   - packages the application and platform Helm charts at the release version, pushes them to `oci://ghcr.io/<owner>/charts/aster` and `oci://ghcr.io/<owner>/charts/aster-platform`, and attaches `aster-<version>.tgz` and `aster-platform-<version>.tgz` to the release,
   - cross-compiles the `aster` CLI for Linux and macOS on amd64 and arm64, attaches `aster-<tag>-<target>` for each target, an exact source archive, a machine-readable release manifest, and `SHA256SUMS`,
   - waits for the matching engine, remote-fixer, and Agent Sandbox Fix executor images and verifies their embedded source revision before publishing charts, the GitHub Release, or the stable major alias,
   - for a **stable** tag only, fast-forwards the `vMAJOR` alias after both charts are packaged, pushed, and attached successfully.

   In parallel, `.github/workflows/image.yml` publishes only the exact release tag for the application, remote fixer, and Agent Sandbox Fix executor. The Fix executor is published for `linux/amd64` at `ghcr.io/<owner>/aster/agent-sandbox-fix-executor`; deployed Agent Sandbox configuration still requires the resolved OCI digest. The git-only remote fixer is published at `ghcr.io/<owner>/aster/remote-fixer` for dashboard-side patch reconstruction and contains neither OpenCode nor model credentials.

The `backend/` tag does not match the release or image workflow triggers, so it does not publish a second GitHub Release or duplicate OCI artifacts. To inspect tag state without changing it, run the publishing script with `RELEASE_DRY_RUN=true`. To recover only a missing module tag without publishing artifacts, use `RELEASE_TAGS_ONLY=true`; that mode is also the gate the image workflow uses before pushing version-tagged images, so it enforces the forward-only rule. Both modes still reject invalid versions, moved tags, and mismatched tag pairs.

4. After both tags and release artifacts are published and the onboarding contract passes at that exact tag, update `docs/supported-onboarding-release.txt`, current onboarding examples, and the setup skill in a follow-up change. Run `make check-onboarding-release-pins`; the guard requires both tags to exist and identify the same commit. An older supported tag may be retained when maintainers explicitly record that compatibility boundary.

## Pre-release to stable

Iterate pre-releases until the release is solid, then cut the stable tag:

```
v1.0.0-beta.1  ->  v1.0.0-beta.2  ->  v1.0.0-rc.1  ->  v1.0.0
```

Pre-releases never move the `vMAJOR` alias and are never marked "latest", so a consumer on `@v1` is unaffected until `v1.0.0` ships. Test a pre-release by pinning a consumer to the exact tag (e.g. `@v1.0.0-beta.1`).

Each tag must move the line forward. The publisher rejects a version that is not the newest in the repository, so an accidental return to an older line, or a prerelease of a version that already shipped, fails before anything is published.

## Release branches (backports)

While everything ships from `main`, no release branch is needed.

Publishing a patch for an older major is **not currently supported by the release automation**. Both the release and image workflows enforce the forward-only rule, so a tag such as `v1.4.1` pushed after `v2.0.0` exists is rejected before anything is published. Supporting it means giving both workflows a manual path that carries the backward-release confirmation through to the publishing script; that path does not exist today, so do not document or promise a backport until it does.

`RELEASE_ALLOW_BACKWARD=true` exists for one narrow local operation: recovering a missing `backend/` module tag on an already-published older release, with `RELEASE_TAGS_ONLY=true`. That combination exits before any artifact is published. Never set it to work around an accidental tag; delete the tag instead.

## Building images from a branch

Every push to `main` publishes the application, remote fixer, and Agent Sandbox Fix executor at `sha-<short>` for that commit, leaving release tags and the `vMAJOR` alias untouched. Pin a deployment to that tag to test a main commit without cutting a release.

For any other branch, run the `Image` workflow manually (Actions -> Image -> Run workflow) against it:

```bash
gh workflow run image.yml --ref my-branch
```

Use the resulting `sha-<short>` tag to pin a deployment for testing. The Agent Sandbox Fix executor is still pinned by digest, so resolve it after the run:

```bash
docker buildx imagetools inspect \
  ghcr.io/<owner>/aster/agent-sandbox-fix-executor:sha-<short> \
  --format '{{.Manifest.Digest}}'
```

## Rolling back

A bad release: cut a new patch with the fix. To stop consumers pulling a broken stable, point the `vMAJOR` alias back at the last good tag:

```bash
git tag -f v1 v1.3.4        # last known-good
git push origin -f refs/tags/v1
```

Consumers pinned to an exact tag are unaffected.

## Retracting a release

Deleting a tag does not un-publish a release, and the pieces come apart in ways that matter.

**The Go module is permanent.** Once anything has fetched `github.com/willie-yao/aster/backend@<tag>`, `proxy.golang.org` caches that version and its checksum forever. Deleting the git tag does not remove it: `go install ...@<tag>` keeps working from the proxy. This is verifiable at any time:

```bash
curl -s https://proxy.golang.org/github.com/willie-yao/aster/backend/@v/list
```

The consequence is the rule that matters most here: **never reuse a version number for different content.** Re-tagging a deleted version at a new commit makes the proxy serve the old code, and anyone whose checksum database disagrees gets a security error rather than a clean failure. If a tag was wrong, burn the number and move to the next one. The forward-only guard in the publisher enforces this.

To tell Go tooling not to select a published version, add a `retract` directive to `backend/go.mod` and release it in a later version:

```go
retract (
    v1.2.3 // Published without the fix in #123; use v1.2.4.
)
```

`go list -m -versions` then hides it, and a consumer already on it is told to upgrade. A retraction is itself shipped as a release, so it must be a forward version.

**Images and charts are separate.** Deleting a git tag leaves `ghcr.io/<owner>/aster:<tag>`, the remote fixer, the Fix executor, and both OCI charts published and pullable. Removing those means deleting the package versions from GHCR directly, which needs a token with `delete:packages`. Deleting a chart version breaks any GitOps deployment pinned to it, so repoint consumers first.

**The GitHub Release is the only cheap part.** It can be deleted or edited freely; nothing resolves against it except the CLI asset download URLs.
