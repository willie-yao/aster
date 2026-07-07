# Agent-proposed fix PRs

The dashboard can draft a **minimal code fix** for a systemic recurring failure
and open a **draft pull request** against the source repo. It is **off by
default**, opt-in per project, and heavily guardrailed: draft-only, bounded file
scope, a CLA-signed commit author, and idempotent dedup.

This is the highest-risk automation the engine offers (it writes code to a repo),
so read this whole page before enabling it.

## What it does

After each fetch, for every **systemic** recurring pattern (the same ones
surfaced on the home page) at or above `min_confidence` that carries a concrete
suggested fix, the engine:

1. **Locates** the target file(s) by choosing from the source repo's **real file
   tree** (fetched and keyword-ranked against the failure), so the model can't
   invent a path that doesn't exist.
2. **Fetches** their current content at a pinned commit.
3. Asks the model for **anchored search/replace edits** (a verbatim snippet to
   find and its replacement), and applies each edit only if its anchor matches
   **exactly once** in the file. Anything ambiguous or not found is rejected, so
   a fix is never applied fuzzily.
4. **Parse-checks** each edited file (Go, YAML, JSON) and drops the fix if an
   edit left it syntactically broken. Go files also get a best-effort in-process
   type check; it skips when the file's imports or symbols can't be resolved in
   the deploy runner (the source module isn't downloaded and sibling package
   files aren't fetched), so a good fix is never dropped for lack of context, and
   only a definite type error in a fully resolved file fails.
5. **Reviews** the change with a second LLM call (a skeptical reviewer that flags
   wrong logic, undefined symbols, or a change that doesn't address the root
   cause). On objections it re-prompts the edit step up to `critique_retries`
   times, then drops the fix.
6. Opens a **draft PR** via fork-and-PR with the change, the rendered diff, and a
   review checklist in the body.

A fix that can't be grounded at any step (no such file, anchor doesn't match,
touches more than `max_files`, no longer parses, or the reviewer keeps rejecting
it) is dropped and logged. No partial or speculative changes are ever pushed.

> **Note on correctness.** The engine verifies the edit is *safe* (real file,
> unique anchor, minimal scope, still parses, and type-checks where resolvable)
> and runs an LLM review, but it does
> not compile the change, run tests, or guarantee it fixes the failure. A fix PR
> is a reviewed **draft starting point**, not a verified patch; Prow CI and a
> human reviewer are the correctness gate (a draft PR won't run CI or merge
> without a maintainer's approval).

## Two modes: fork-and-PR vs direct

How the fix branch reaches the source repo depends on whether you can write to
it, controlled by `ai.fix_prs.fork` (default `true`):

- **`fork: true` (default) — fork-and-PR.** For a source repo you **don't** own
  (the usual case: an upstream community repo). The engine forks the repo under
  the token's identity, pushes the branch to that fork, and opens a **cross-fork
  PR** against the source repo.
- **`fork: false` — direct.** For a source repo you **do** own or maintain (e.g.
  a team running the dashboard on its own CI). The engine pushes the branch
  straight to the source repo and opens a **same-repo PR**. No fork involved.

Either way the PR targets the source repo's default branch and is opened as a
draft. The branch is **never** pushed into a repo you don't own.

## Identity, CLA, and the token (read this first)

- **`FIX_TOKEN`** is a **personal access token** of a real contributor. It is
  **not** the Actions `GITHUB_TOKEN` (which can't touch a fork elsewhere). Which
  PAT kind you need depends on the mode:
  - **`fork: true` against a repo you don't own** → use a **classic PAT** (scope
    `repo`, or `public_repo` for public-only repos). A **fine-grained PAT cannot
    open a PR against a repo you don't own**, because it can only be granted
    permissions on your own repos.
  - **`fork: false` against a repo you own** (or `fork: true` testing against
    your own fork) → a **fine-grained PAT** works: scope it to that repo with
    **Contents: Read and write** and **Pull requests: Read and write**.
- **CLA / DCO.** CNCF projects (Kubernetes, etc.) run EasyCLA, which checks
  **every commit's author** against a signed CLA and blocks merge otherwise. So:
  - `author_name` / `author_email` **must** be the CLA-signed identity, and the
    email **must** match that GitHub account, or the check reports an "unknown
    commit author".
  - Every commit gets a DCO `Signed-off-by` trailer matching the author
    (required by Kubernetes repos). The engine adds this automatically.
  - A GitHub App / bot identity generally is **not** recognized by EasyCLA;
    use a human contributor's PAT.
- **Prow keeps a human in the loop for free.** A draft PR won't run CI or merge
  without a maintainer's `/ok-to-test`, `/lgtm`, and `/approve`. The engine never
  merges anything.

## Configuration

```yaml
ai:
  fix_prs:
    enabled: true
    # repo:                       # defaults to branding.source_repo
    #   owner: "kubernetes-sigs"
    #   name: "cluster-api-provider-azure"
    author_name: "Jane Maintainer"     # required: CLA-signed identity
    author_email: "jane@example.com"   # required: must match that GitHub account
    # fork: true                  # true (default): fork-and-PR for a repo you don't own;
    #                             # false: direct branch + same-repo PR for a repo you own
    # min_confidence: high        # only systemic patterns at >= this confidence (default high)
    # max_files: 3                # cap files a single fix may touch (default 3)
    # max_new_per_run: 1          # cap fix PRs per fetch (default 1)
    # labels: [ai-proposed-fix]   # labels applied to each PR
    # dry_run: false              # propose without opening a PR (see below)
    # critique_retries: 1         # LLM review re-prompts before dropping (default 1; 0 disables)
    # critique_endpoint: "..."    # optional: review with a different chat-completions endpoint
    # critique_model: "..."       # optional: review with a different model (e.g. a stronger one)
```

`enabled: true` requires `author_name` and `author_email` (validated at load).
The feature is active only when **all** of `enabled: true`, a non-empty
`FIX_TOKEN`, and a resolved source repo are present; any missing piece is a
no-op, never a deploy failure.

### The LLM review (`critique_retries`)

After the edit parses, a second LLM call reviews it as a skeptical reviewer and
returns concrete defects (not style). If it objects, the engine re-prompts the
edit step with that feedback, up to `critique_retries` times (default 1), then
drops the fix. Set `critique_retries: 0` to skip the review.

By default the review uses the **same** provider as generation. To review with a
different (e.g. stronger) model, set `critique_endpoint` / `critique_model` (or
the `FIX_CRITIQUE_ENDPOINT` / `FIX_CRITIQUE_MODEL` env vars), with the token in
`FIX_CRITIQUE_TOKEN` (falls back to `AI_TOKEN`). A separate reviewer is also more
independent than the model grading its own work.

Wire the token into the deploy workflow:

```yaml
jobs:
  deploy:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-deploy.yml@main
    secrets:
      AI_TOKEN: ${{ secrets.AI_TOKEN }}
      FIX_TOKEN: ${{ secrets.FIX_TOKEN }}
```

## Start with dry-run

Before letting it open real PRs, set `dry_run: true`. The engine runs the full
pipeline (locate, fetch, edit, validate) and writes the proposed changes to
`fix_previews.json` in the published data directory (and logs the diffs), but
**opens no PR and forks nothing**. Inspect the previews, confirm the edits look
right and target the correct files, then flip `dry_run` off.

## Following the repo's PR template

When the source repo has a pull-request template (`.github/PULL_REQUEST_TEMPLATE.md`,
`PULL_REQUEST_TEMPLATE.md`, or `docs/PULL_REQUEST_TEMPLATE.md`), the engine
reformats the generated PR description to follow it with one extra AI call: it
fills the template's sections from the proposed change, keeps placeholder text
and checklists you have no information for, and picks a single best-fit Prow
`/kind` line when the template has one. The warning banner, rendered diff,
dashboard link, and dedup marker are always preserved. No template (or no AI
configured) falls back to the default body, and any error during reformatting
silently uses the default. This is automatic; there is no flag to set. Fetching
the template uses `FIX_TOKEN`, which already has Contents read on the source repo.

## Guardrails (summary)

- **Opt-in** per project; **draft-only** PRs; never pushes to a protected branch.
- Only **systemic**, at-or-above-`min_confidence` patterns with a concrete fix.
- **Anchored edits**, exact-match-once or rejected; bounded by `max_files`.
- Dedicated **`FIX_TOKEN`** with a CLA-signed author and DCO sign-off.
- **Idempotent**: a hidden marker keyed by job + root-cause fingerprint (local
  state plus an open-PR search) means a pattern is never proposed twice, and a
  different cause on the same job is proposed separately.
- **`max_new_per_run`** caps PRs per fetch.

## Known limitations

- **File mode.** Edited files are committed as regular files (`100644`). If a fix
  were to edit an executable script, the PR would drop the executable bit; the
  change is visible in the draft diff for a reviewer to catch. Fix targets are
  typically YAML/templates, so this is rare.
- **Concurrency.** Dedup (local state + an open-PR search) is not atomic, so two
  overlapping deploys could both propose the same fix. Scheduled deploys are
  normally serialized; add a workflow `concurrency:` group if you run them in
  parallel.
- **First fork.** Creating a brand-new fork is asynchronous; on the very first
  run for a never-forked repo the commit step may fail while the fork populates.
  The next run (fork now exists) succeeds.

## Relationship to the other features

This builds on the same pattern analysis that drives the home-page recurring
patterns, the auto-filed issues ([github-issues.md](github-issues.md)), and the
skill suggestions ([skills.md](skills.md#auto-suggesting-recipes)). Issues and
skill suggestions act on **your** repos; fix PRs are the only feature that writes
to the **source** repo, which is why the identity and CLA requirements are
stricter.
