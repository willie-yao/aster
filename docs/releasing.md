# Releasing the engine

How to cut a release of the Aster engine. Consumers on the GitHub Actions + Pages path pin the engine through the reusable deploy workflow, so for them a "release" is just a git tag plus a GitHub Release. A tag also publishes the Kubernetes-native artifacts: the container image and the Helm chart.

## Versioning

[Semantic Versioning](https://semver.org), tags prefixed with `v`:

- `vMAJOR.MINOR.PATCH` for stable releases (e.g. `v1.2.0`).
- `vMAJOR.MINOR.PATCH-beta.N` / `-rc.N` for pre-releases (e.g. `v1.0.0-beta.1`).
- `backend/vMAJOR.MINOR.PATCH[-PRERELEASE]` pairs every root release tag with the same exact commit so the nested Go module resolves at that version.
- A moving `vMAJOR` alias (e.g. `v1`) tracks the highest stable release in that major. An older minor-line patch never moves it backward.

Before the first stable release in a major, the moving alias does not exist. Consumers must use `@main`, a commit SHA, or an exact prerelease tag that is already published. Do not document `@v1` as usable until `v1.0.0` exists.

See [CHANGELOG.md](../CHANGELOG.md) for what bumps major/minor/patch. Note that internal critique-version bumps and stronger investigation floors can force re-analysis on upgrade and are therefore at least a minor bump; call them out in the release notes.

## Release notes

Each release has one notes file named for its tag, `changelog/<tag>.md`, listed in the `CHANGELOG.md` index. The file holds the notes body alone, without a version heading of its own. It is published verbatim as the GitHub Release body, so the release page shows the curated notes rather than a generated commit list. The publisher refuses to release a tag whose notes file is missing, empty, or absent from the index.

Notes are assembled from the `release-note` blocks of the pull requests merged in the release line. A prerelease covers everything since the previous tag; a stable release covers everything since the previous *stable* tag in that line. Maintenance-patch notes cover fixes backported to that minor line, not unrelated changes on `main`.

Because the notes are published verbatim as the release body, relative paths in them do not resolve against the repository. Link repository files with absolute URLs pinned to the tag, such as `https://github.com/willie-yao/aster/blob/<tag>/docs/<file>.md`. Rewrite any relative or root-relative link copied out of a `release-note` block.

## Cutting a release

1. Make sure the source branch is green. Write `changelog/<tag>.md` from the `release-note` blocks merged in that line, and add the release to the index in `CHANGELOG.md`. Adding a new indexed notes file in a pull request triggers the read-only **Release PR preflight** check. It validates the version, target branch, notes, index, tag availability, and Go proxy without App credentials or tag writes. A passing PR check does not reserve the version or start a release. Keep `docs/supported-onboarding-release.txt`, current onboarding examples, and the setup skill pinned to the last published release.
2. Create the release tag pair by running the **Release tag** workflow (Actions -> Release tag -> Run workflow) on `main` for a new version line, or on a maintained `release/MAJOR.MINOR` branch for a patch in that line. It validates again before it tags: the version is well formed, `changelog/<tag>.md` exists with real content and is indexed in `CHANGELOG.md` under that exact tag, the version moves forward globally on `main` or within the maintained line, neither tag exists yet, and the clean checkout is the reviewed tip of the selected branch. A maintenance branch must descend from its stable `.0` tag. Only then does it create both tags at that commit and push them atomically.

   ```bash
   gh workflow run release-tag.yml --ref main -f version=v1.0.0-beta.1
   ```

   Run it with `-f dry_run=true` first to validate without tagging. Run it again without that flag only after the dry run succeeds. A PR preflight is not a substitute: the source branch and version may have changed since the PR was checked. Tagging by hand skips these checks, which is how this repository once published a release whose module tag was never created, and a version line that moved backward.

   The workflow pushes with a GitHub App installation token from the `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` secrets, because GitHub suppresses workflow runs triggered by the default `GITHUB_TOKEN`: tags pushed with it would exist and publish nothing. Without those secrets the workflow fails before tagging.
3. The `Release` workflow (`.github/workflows/release.yml`) runs on the tag:
   - re-runs the full CI gate against the tagged commit,
   - verifies both release tags identify the reviewed commit; if the root tag exists and only the module tag is missing, it creates the module tag with a non-force push before publishing,
   - requires a main-line version to exceed all earlier versions, or a patch on `release/MAJOR.MINOR` to exceed the versions in that minor line, comparing by semantic precedence rather than string order,
   - creates the GitHub Release from `changelog/<tag>.md` (marked **pre-release** when the tag has a `-beta`/`-rc` suffix); an older-line patch is not marked Latest,
   - packages the application and platform Helm charts at the release version, pushes them to `oci://ghcr.io/<owner>/charts/aster` and `oci://ghcr.io/<owner>/charts/aster-platform`, and attaches `aster-<version>.tgz` and `aster-platform-<version>.tgz` to the release,
   - cross-compiles the `aster` CLI for Linux and macOS on amd64 and arm64, attaches `aster-<tag>-<target>` for each target, an exact source archive, a machine-readable release manifest, and `SHA256SUMS`,
   - attests build provenance for every asset named in `SHA256SUMS` and, from the image workflow, for each published image digest,
   - waits for the matching engine, remote-fixer, and Agent Sandbox Fix executor images and verifies their embedded source revision before publishing charts, the GitHub Release, or the stable major alias,
   - promotes the three verified image digests to OCI `latest` only for the highest stable release across all lines, then advances `vMAJOR` only when that release is the highest stable in its major.

   In parallel, `.github/workflows/image.yml` publishes only the exact release tag for the application, remote fixer, and Agent Sandbox Fix executor. It does not update OCI `latest`; the serialized Release publication handles that alias after verifying the images. The Fix executor is published for `linux/amd64` at `ghcr.io/<owner>/aster/agent-sandbox-fix-executor`; deployed Agent Sandbox configuration still requires the resolved OCI digest. The git-only remote fixer is published at `ghcr.io/<owner>/aster/remote-fixer` for dashboard-side patch reconstruction and contains neither OpenCode nor model credentials.

The `backend/` tag does not match the release or image workflow triggers, so it does not publish a second GitHub Release or duplicate OCI artifacts. To inspect tag state without changing it, run the publishing script with `RELEASE_DRY_RUN=true`. To recover only a missing module tag without publishing artifacts, use `RELEASE_TAGS_ONLY=true`; that mode is also the gate the image workflow uses before pushing version-tagged images, so it enforces the same branch and version rules. Both modes still reject invalid versions, moved tags, and mismatched tag pairs.

4. After both tags and release artifacts are published and the onboarding contract passes at that exact tag, update `docs/supported-onboarding-release.txt`, current onboarding examples, and the setup skill in a follow-up change. Run `make check-onboarding-release-pins`; the guard requires both tags to exist and identify the same commit. An older supported tag may be retained when maintainers explicitly record that compatibility boundary.

## Pre-release to stable

Iterate pre-releases until the release is solid, then cut the stable tag:

```
v1.0.0-beta.1  ->  v1.0.0-beta.2  ->  v1.0.0-rc.1  ->  v1.0.0
```

Pre-releases never move the `vMAJOR` alias and are never marked "latest", so a consumer on `@v1` is unaffected until `v1.0.0` ships. Test a pre-release by pinning a consumer to the exact tag (e.g. `@v1.0.0-beta.1`).

Each tag must move its line forward. A new version from `main` must also exceed every tag in the repository; an older maintained line can only publish its next patch from its matching release branch. A prerelease of a version that already shipped fails before anything is published.

## Verifying a release

Every release payload named in `SHA256SUMS`, and every published image, carries a signed build provenance attestation, so a consumer can check where an artifact was built rather than trusting a checksum they recorded by hand. `SHA256SUMS` itself is not attested, since it cannot contain its own digest.

A downloaded CLI binary, chart, or source archive:

```bash
gh attestation verify aster-v1.0.0-linux-amd64 \
  --repo willie-yao/aster \
  --signer-workflow willie-yao/aster/.github/workflows/release.yml
```

An image, by the digest a deployment pins:

```bash
gh attestation verify oci://ghcr.io/willie-yao/aster@sha256:<digest> \
  --repo willie-yao/aster \
  --signer-workflow willie-yao/aster/.github/workflows/image.yml
```

`--repo` alone only proves some workflow in this repository signed the artifact. `--signer-workflow` is what pins it to the release automation, so prefer both. Add `--source-ref refs/tags/<tag>` to additionally require a specific release.

Both commands fetch the attestation from GitHub. Image attestations are also pushed to the registry beside the image; pass `--bundle-from-oci` to fetch from there instead, which needs registry access rather than access to this repository.

Provenance records the workflow, repository, and commit that produced the artifact. It does not assert the artifact is correct or safe, only where it came from.

## Release branches (backports)

Do not create a branch for every release. When a minor line needs concurrent support, create `release/MAJOR.MINOR` from its latest stable tag and protect it with reviews and CI. Backport fixes through pull requests, usually by cherry-picking a fix already merged to `main`. Keep release notes and the changelog index on that branch. Its tags must be patch versions (including optional beta/rc candidates) and move forward within that minor line.

For example, after `v1.5.0` ships from `main`, prepare `v1.4.1` on `release/1.4`. Merge its release-notes PR into `release/1.4`, then run:

```bash
gh workflow run release-tag.yml --ref release/1.4 -f version=v1.4.1 -f dry_run=true
gh workflow run release-tag.yml --ref release/1.4 -f version=v1.4.1
```

The branch must contain the current release infrastructure. If it was cut from an older tag, backport those workflow and script changes first. Publication permits the branch to advance after the tag was created but still requires the tagged commit to be reachable from it. An older patch does not move GitHub Latest or OCI `latest`; it moves `vMAJOR` only if it is the highest stable release in that major. Roll back a consumer through a reviewed pin change rather than reusing a published tag.

`RELEASE_ALLOW_BACKWARD=true` remains limited to recovering a missing `backend/` module tag on an already-published older release, with `RELEASE_TAGS_ONLY=true`. It is not the backport path.

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
