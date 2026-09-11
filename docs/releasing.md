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
2. Create the release tag pair by running the **Release tag** workflow (Actions -> Release tag -> Run workflow) with the version, for example `v1.0.0-beta.1`. It validates before it tags: the version is well formed, `changelog/<tag>.md` exists with real content and is indexed in `CHANGELOG.md` under that exact tag, the version moves the line forward, neither tag exists yet, and the checkout is the reviewed tip of `main`. Only then does it create both tags at that commit and push them atomically.

   ```bash
   gh workflow run release-tag.yml -f version=v1.0.0-beta.1
   ```

   Run it with `-f dry_run=true` first to validate without tagging. Tagging by hand skips every one of those checks, which is how this repository once published a release whose module tag was never created, and a version line that moved backward.

   The workflow pushes with a GitHub App installation token from the `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` secrets, because GitHub suppresses workflow runs triggered by the default `GITHUB_TOKEN`: tags pushed with it would exist and publish nothing. Without those secrets the workflow fails before tagging.
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

**Deleting a tag does not reliably withdraw the Go module.** Once `proxy.golang.org` has fetched `github.com/willie-yao/aster/backend@<tag>`, it may keep serving that version from its cache even though the tag, and even the whole repository, is gone. The mirror does this deliberately, to avoid breaking builds that already depend on it. All 13 deleted `v0.9.0-rc.*` versions were still listed here well after their tags were removed:

```bash
curl -s https://proxy.golang.org/github.com/willie-yao/aster/backend/@v/list
```

The consequence is the rule that matters most here: **never reuse a version number for different content.** A version is identified by a checksum that does not change, so after re-tagging, some clients keep getting the original cached bytes while any client that fetches the rebuilt content fails verification with a security error rather than a clean failure. If a tag was wrong, burn the number and move to the next one. The upstream advice is the same: publish a new version instead.

The **Release tag** workflow enforces the part it can see: it refuses a version the module mirror is currently serving. It cannot see a version that was never fetched before its tags were deleted, so treat never reusing a version as an operating rule rather than something the tooling guarantees.

To tell Go tooling not to select a published version, add a `retract` directive to `backend/go.mod` and release it in a later version:

```go
retract (
    v1.2.3 // Published without the fix in #123; use v1.2.4.
)
```

`go get` and `go list -m -u all` then report the retraction to a consumer already on that version, and it stops being selected as a latest or upgrade candidate. A retraction is itself shipped as a release, so it must be a forward version.

**Images and charts are separate.** Deleting a git tag leaves `ghcr.io/<owner>/aster:<tag>`, the remote fixer, the Fix executor, and both OCI charts published and pullable. Removing those means deleting the package versions from GHCR directly, which needs package-admin access: the web UI, or a token with `delete:packages` for the API. A public version with more than 5,000 downloads cannot be deleted without GitHub Support. Deleting a chart version breaks any GitOps deployment pinned to it, so repoint consumers first.

**The GitHub Release is the only cheap part.** It can be deleted or edited freely; nothing resolves against it except the CLI asset download URLs.
