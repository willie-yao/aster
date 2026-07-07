# Auto-filing GitHub issues

The dashboard can open and maintain GitHub issues for its highest-signal
findings: **systemic recurring patterns** and **persistent test failures**. It
is **off by default** and opt-in per project.

Each finding maps to one issue. The engine reuses the same issue across runs
(no duplicates), and when a finding stops recurring it posts a "recovered"
comment (and optionally closes the issue).

## What triggers an issue

| Trigger | Source | Condition |
|---|---|---|
| `patterns` | cross-build pattern analysis | a job's recent failures share one **systemic** root cause |
| `persistent` | flakiness report | a test failed in **≥3 consecutive** runs |

Both are already computed by the fetcher; issues are just a delivery channel for
them, alongside the existing Slack/Teams notifications.

## Permissions (read this first)

The deploy runs in your **consumer** repo with its `GITHUB_TOKEN`, which can only
act on that repo. Filing issues on a **different** repo (e.g. the upstream
project under `branding.source_repo`) requires a separate token:

- A **fine-grained PAT** or a **GitHub App installation token** with
  `issues: write` on the **target** repo, provided as the `ISSUE_TOKEN` secret.
  To follow the target repo's issue template (see below), the token also needs
  `contents: read`; without it, template-following is skipped and issues use the
  default body.
- You must actually have rights to open issues there. Auto-filing bot issues on
  an upstream community repo is usually unwanted, so point `issues.repo` at a
  repo **you control** (your consumer repo, or a dedicated tracking repo) unless
  you specifically intend otherwise.

The feature is active only when **both** `issues.enabled: true` **and** a
non-empty `ISSUE_TOKEN` are present. Either missing is a no-op, never a deploy
failure. Per-issue API errors (403/404/rate limit) are logged and skipped.

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
  triggers: [patterns, persistent]   # default: both
  labels: [prow-dashboard]           # default: [prow-dashboard]
  comment_on_recovery: true          # default true: comment when a finding clears
  close_on_recovery: false           # default false: leave the issue open
  max_new_per_run: 5                 # default 5: cap issues created per fetch
```

Wire the token in the deploy workflow:

```yaml
# .github/workflows/deploy.yml  (GitHub Actions + Pages path)
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@v1
    # ...
    secrets:
      ISSUE_TOKEN: ${{ secrets.ISSUE_TOKEN }}
```

On the [Kubernetes-native](kubernetes.md) path, set `ISSUE_TOKEN` on the worker
instead, via `fetcher.extraEnv` in the Helm values (sourced from a Secret). The
same scheduled auto-filing runs there; and an admin can additionally file a
single issue on demand from the UI (see [server.md](server.md#admin-gated-actions)).

## How dedup works

Every filed issue body ends with a hidden marker:

```
<!-- prow-ai-dashboard-key:<hash> -->
```

The engine tracks filed issues two ways, so it never opens a duplicate:

1. **Local state** (`issue_state.json`, persisted with the rest of the data
   cache): maps each finding to its issue number. In the steady state (finding
   still active, issue already filed) this means **zero** API calls.
2. **Repo-side search** (eviction-proof): when local state doesn't know a
   finding, the engine searches the target repo for an **open** issue carrying
   that finding's marker before creating one. So even if the data cache is
   evicted, an existing open issue is **adopted**, not duplicated.

## Lifecycle

- **New finding** → file an issue (title, AI root cause / suggested fix,
  affected builds linked to the dashboard, the hidden marker), up to
  `max_new_per_run` per fetch.
- **Still active** → do nothing (already tracked).
- **Recovered** (no longer a pattern / no longer persistent) → post a recovery
  comment (if `comment_on_recovery`) and close the issue (if
  `close_on_recovery`), then stop tracking it.

Recovery is automatic: pattern verdicts and persistent-failure status are
recomputed every fetch from the most recent builds, so once a job goes green the
finding drops out and its issue is resolved on the next run. Recovery is scoped
to the triggers you have enabled, so turning a trigger off leaves its existing
issues untouched (it does not mass-resolve them); and changing `issues.repo`
resets the local tracking state so issue numbers are never mixed across repos.

## Following the repo's issue template

When the target repo has one or more Markdown issue templates (under
`.github/ISSUE_TEMPLATE/`, or a legacy `.github/ISSUE_TEMPLATE.md`), the engine
reformats a new issue with one extra AI call: it picks the best-fit template,
fills its sections from the finding, keeps placeholder text and checklists it has
no information for, and chooses a single `/kind` line when the template has one.
The hidden dedup marker is always preserved so tracking and adoption keep
working. YAML issue *forms* are skipped (only `.md` templates are followed). No
template (or no AI configured) falls back to the default body, and any error
during reformatting silently uses the default. This is automatic; there is no
flag to set.

## Implementation reference

- `backend/internal/issues/` — the GitHub client, the reconciler (state +
  repo-side dedup), and the spec builder that turns findings into issues.
- Wired in `backend/internal/fetcher/fetcher.go` (Step 7), gated on
  `issues.enabled` + `ISSUE_TOKEN`.
