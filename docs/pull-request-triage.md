# Pull request triage

Aster can publish a view of presubmit results for the open pull requests of `branding.source_repo`. The deterministic pass answers which pull requests have failing checks, what failed, and what observed evidence rules each pull request in or out as the likely source.

Pull request triage is available on GitHub Pages and Kubernetes. Optional bot comments and server-side AI escalation are separate capabilities with separate credentials.

## Enable triage

Add the portable configuration to `project.yaml`:

```yaml
pull_requests:
  enabled: true
  max: 100
  builds_per_job: 3
```

| Field | Default | Purpose |
| --- | ---: | --- |
| `enabled` | `false` | Publish the pull request index, detail pages, and shared-failure view. |
| `max` | `100` | Maximum open pull requests per pass, newest updates first. |
| `builds_per_job` | `3` | Presubmit builds examined before selecting the newest applicable build. |

`discovery.include_presubmits` is not required. Triage resolves presubmits from the job catalog independently. Enable presubmit discovery only when those jobs should also appear in the main job dashboard. Fetch commands do not override this project setting.

Draft pull requests are excluded. Each successful pass writes:

```text
pull-requests.json
pull-requests/<number>.json
pull-request-failures.json
```

A triage failure does not abort normal dashboard publication. The previous pull request view remains available and the logs report the failure.

## Read credential

Triage reads the GitHub API on every pass.

| Deployment | Credential |
| --- | --- |
| GitHub Pages | The reusable workflow supplies `GITHUB_TOKEN`. |
| Kubernetes | Configure `ai.githubReadTokenSecretName` and, when needed, `ai.githubReadTokenSecretKey`; or supply `GITHUB_READ_TOKEN`. |

For a public repository, the credential needs no repository privileges. It is used to avoid the low anonymous rate limit. Private repositories require the minimum read access needed for repository metadata and changed files. The credential is required whether or not AI analysis is enabled.

`aster onboard doctor` warns when triage is enabled without an authenticated GitHub read credential. See [Pull request triage stops updating](troubleshooting.md#pull-request-triage-stops-updating) for the operational symptom.

## Deterministic attribution

Each failing test receives one attribution based only on observed results:

| Verdict | Meaning |
| --- | --- |
| `pre_existing` | The same test is already failing on the base branch. |
| `widespread` | The same job and test fails on at least two other open pull requests targeting the same base branch. |
| `known_flake` | Base-branch history classifies the test as flaky. |
| `touches_changed_code` | Nothing else explains the failure and a reported source location overlaps a changed file. |
| `unexplained` | No observed evidence rules the pull request out. |
| `inconclusive` | No comparable base-branch data was available. |

The verdict never claims that a pull request caused a failure. Observations can rule a pull request out, but they cannot prove causation.

Important boundaries:

- Attribution uses base-branch history only. Presubmit history belongs to other pull requests and cannot establish the baseline.
- Cross-pull-request matching includes base branch, job name, and test name.
- One peer is retained as evidence but does not produce `widespread`; two peers are required.
- Source overlap uses JUnit-reported locations and current changed files. It is omitted when the build is stale, changed files are truncated or unavailable, or the location points outside the checked-out repository.
- A build that tested an older pull request head is marked stale.
- A failed check without a failing JUnit case receives one synthesized `Prow job execution` failure. It is compared with the same job on peers, not with base-branch tests by the generic name.
- When build artifacts have expired, the check is `UNKNOWN` even if GitHub still retains its commit status.

## Shared failures

`pull-request-failures.json` groups the same failure across open pull requests so maintainers do not have to follow links between peer pages.

A shared failure:

- requires at least two members;
- is keyed by base branch, job name, and test name;
- has a stable ID derived from that key;
- records each member pull request, build, and attribution;
- reports only the oldest and newest member builds observed in the current pass;
- is `escalatable` only when no member already offers the cheaper single-pull-request escalation path.

Clustering uses no model calls and is published by both deployment paths.

## Optional bot comment

Aster can post one comment on each newly observed pull request, linking to its triage page. This is an unattended contributor-facing write, so it is off by default and starts in dry-run mode.

```yaml
pull_requests:
  enabled: true
  comment:
    enabled: true
    dry_run: true
    max_per_pass: 10
```

| Field | Default | Purpose |
| --- | ---: | --- |
| `comment.enabled` | `false` | Enable the commenting pass. Requires triage. |
| `comment.dry_run` | `true` | Log exact comment bodies without posting. Only explicit `false` permits writes. |
| `comment.max_per_pass` | `10` | Bound comments in one pass. |

### GitHub App identity

Comments use a repository-installed GitHub App, not a user token or a shared Aster bot. Create an App with only:

- **Issues: Read and write**, because pull request comments use the issues API;
- **Pull requests: Read-only**, needed to inspect private pull requests;
- no webhook subscriptions.

Install the App on `branding.source_repo`, record its App ID, and create a private key. Installation requires repository administration or the corresponding organization approval.

Supply `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY` as repository secrets on Pages, or as Secret-backed `fetcher.extraEnv` entries on Kubernetes. The fetcher logs the resolved bot identity before the pass. Review dry-run output before setting `dry_run: false`.

### Comment selection and deduplication

Aster comments only when the current pass published a page for every open pull request. If `pull_requests.max` truncated the listing, the whole commenting pass is skipped to avoid creating links to pages that may be pruned.

The pass skips:

- every pull request that existed when commenting was first enabled;
- pull requests the App already commented on;
- Aster-created Fix pull requests;
- drafts and pull requests that closed, merged, or became drafts during the pass;
- pull requests whose repeated posting failures reached the retry bound;
- work beyond `comment.max_per_pass`.

The activation watermark, local records, and a direct GitHub check protect against duplicates when cached Pages state expires. Published pages retained for posted comments are bounded to 90 days. `-skip-side-effects` and a failed triage pass suppress commenting.

On Pages, the comment is posted during the fetch step before the site deploys. A later workflow failure can temporarily leave a link pointing at the previous site contents. There is no per-pull-request opt-out; disable the feature when a repository does not want these comments.

## Optional AI escalation

Deterministic attribution uses no model calls. A Kubernetes server can optionally analyze a residual `unexplained`, `touches_changed_code`, or `inconclusive` failure on demand:

```yaml
server:
  pullRequestEscalation:
    enabled: true
```

The `server.pullRequestEscalation.enabled` chart value requires `ai.enabled`, `pull_requests.enabled`, and authenticated server settings. Escalation is read-only, does not enable GitHub writes, and does not require `BOT_TOKEN`. Configure the GitHub read token so source and changed files are fetched without anonymous rate limits. Static Pages never exposes the escalation API.

The server reserves admission before reading GitHub or artifacts, runs one pull request or shared-failure escalation at a time, bounds the queue and total lifetime, and shares a result between authenticated maintainers. Provider errors, timeouts, and interrupted work remain retryable.

A pull request escalation:

- is available only after deterministic evidence did not explain the failure;
- rejects stale builds whose tested head differs from the current pull request;
- provides changed files only as investigation context;
- instructs the model not to claim that the pull request caused the failure;
- uses an isolated analysis-cache module and private bounded state.

A shared-failure escalation uses one current member build for artifact evidence and is keyed by the shared failure ID rather than one pull request. It is offered only when no member has a cheaper direct escalation. Its model context names all members but supplies no diff and forbids attributing the failure to any one pull request.

Escalation results remain private. They are separate from the normal dashboard analysis cache and never change deterministic attribution.

## Related references

- [Project configuration](project-configuration.md) for exact portable fields.
- [GitHub Pages](github-pages.md) for workflow credentials and publication.
- [Kubernetes operator reference](kubernetes-reference.md) for Helm settings.
- [Server mode](server.md) for authentication, capabilities, and API endpoints.
- [Troubleshooting](troubleshooting.md#pull-request-triage-stops-updating) for stale-view diagnosis.
