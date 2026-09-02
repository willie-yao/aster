# GitHub issues

The dashboard can open and maintain GitHub issues for its highest-signal findings: **systemic recurring patterns** and **persistent test failures**. It is **off by default** and opt-in per project.

**A maintainer opens every issue.** Nothing files an issue on a schedule. An admin reviews a finding in the dashboard and clicks File issue; the engine then maintains that issue for them. Each finding maps to one issue, the engine reuses the same issue across runs (no duplicates), and when a finding stops recurring the scheduled pass posts a "recovered" comment and optionally closes it.

This makes issues a **server-mode feature**. A static [Pages](github-pages.md) deployment has no authenticated action API, so nothing can file an issue there and `issues.enabled` has nothing to act on.

## What triggers an issue

| Trigger | Source | Condition | Can file? |
|---|---|---|---|
| `patterns` | cross-build pattern analysis | a job's recent failures share one **systemic** root cause | yes |
| `persistent` | flakiness report | a test failed in as many consecutive runs as [`attention.persistent_after`](project-configuration.md#attention-thresholds) requires, default 3 | **no, recovery only** |

`persistent` is retired for filing. The dashboard's File issue action covers systemic patterns and individual builds, so nothing creates a `persistent::` issue any more. Keep the trigger enabled if an earlier engine version filed persistent issues for you: it scopes recovery, so those issues still get their recovery comment and close when the test stops failing. Removing it from `triggers` leaves them open forever instead.

Filing is scoped to the same list. With `triggers: [persistent]` the File issue action refuses a pattern, because recovery would never close what it filed.

Both signals are already computed by the fetcher; issues are just a delivery channel for them, alongside optional [email notifications](notifications.md).

## Credentials (read this first)

Two credentials do two different jobs, and neither is the reusable workflow's `GITHUB_TOKEN`:

| Operation | Where it runs | Credential |
|---|---|---|
| File issue (maintainer-initiated) | dashboard server | `BOT_TOKEN` |
| Comment and close on recovery | worker or fetcher | `ISSUE_TOKEN` |

Both need `issues: write` on the **target** repo, as a **fine-grained PAT** or a **GitHub App installation token**. To follow the target repo's issue template (see below), `BOT_TOKEN` also needs `contents: read`; without it, template-following is skipped and issues use the default body.

You must actually have rights to open issues there. Filing bot issues on an upstream community repo is usually unwanted, so point `issues.repo` at a repo **you control** (your consumer repo, or a dedicated tracking repo) unless you specifically intend otherwise.

Recovery is active only when **both** `issues.enabled: true` **and** a non-empty `ISSUE_TOKEN` are present. Either missing is a no-op, never a deploy failure. Per-issue API errors (403/404/rate limit) are logged and skipped.

## Configuration

```yaml
# project.yaml
issues:
  enabled: true                 # default false
  # repo: the target repo. Defaults to branding.source_repo. Point it at a repo
  # you control; the ISSUE_TOKEN needs issues:write there.
  repo:
    owner: "your-org"
    name: "your-tracking-repo"
  triggers: [patterns, persistent]   # default: both; persistent scopes recovery only
  labels: [prow-dashboard]           # default: [prow-dashboard]
  comment_on_recovery: true          # default true: comment when a finding clears
  close_on_recovery: false           # default false: leave the issue open
```

Wire the credentials on the [Kubernetes-native](kubernetes-reference.md) path: set `BOT_TOKEN` on the server and `ISSUE_TOKEN` on the worker, via `extraEnv` in the Helm values (sourced from a Secret). Admins then file issues from the UI (see [server.md](server.md#admin-gated-actions)) and the worker maintains them.

## How dedup works

Every filed issue body ends with a hidden marker:

```
<!-- prow-ai-dashboard-key:<hash> -->
```

The engine tracks filed issues two ways, so it never opens a duplicate:

1. **Local state** (`issue_state.json`, persisted with the rest of the data cache): maps each finding to its issue number. In the steady state (finding still active, issue already filed) this means **zero** API calls.
2. **Repo-side search** (eviction-proof): when local state doesn't know a finding, the engine searches the target repo for an **open** issue carrying that finding's marker before creating one. So even if the data cache is evicted, an existing open issue is **adopted**, not duplicated.

## Lifecycle

- **New finding** → nothing happens until an admin clicks File issue, which files it (title, AI root cause / suggested fix, affected builds linked to the dashboard, the hidden marker).
- **Still active** → do nothing (already tracked).
- **Recovered** (no longer a pattern / no longer persistent) → post a recovery comment (if `comment_on_recovery`) and close the issue (if `close_on_recovery`), then stop tracking it.

Recovery is automatic: pattern verdicts and persistent-failure status are recomputed every fetch from the most recent builds, so once a job goes green the finding drops out and its issue is resolved on the next run. Recovery is scoped to the triggers you have enabled, so turning a trigger off leaves its existing issues untouched (it does not mass-resolve them); and changing `issues.repo` resets the local tracking state so issue numbers are never mixed across repos.

## Following the repo's issue template

When the target repo has one or more Markdown issue templates (under `.github/ISSUE_TEMPLATE/`, or a legacy `.github/ISSUE_TEMPLATE.md`), the engine reformats a new issue with one extra AI call: it picks the best-fit template, fills its sections from the finding, keeps placeholder text and checklists it has no information for, and chooses a single `/kind` line when the template has one. The hidden dedup marker is always preserved so tracking and adoption keep working. YAML issue *forms* are skipped (only `.md` templates are followed). No template (or no AI configured) falls back to the default body, and any error during reformatting silently uses the default. This is automatic; there is no flag to set.

## Implementation reference

- `backend/internal/issues/`: the GitHub client, `File` (state + repo-side dedup) and `Recover` (comment and close), and the spec builder that turns findings into issues.
- `File` is called by `backend/internal/actions/` for the maintainer action. `Recover` is called by `backend/internal/fetcher/fetcher.go`, gated on `issues.enabled` + `ISSUE_TOKEN`.
