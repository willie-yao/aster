# Experimental Fix PR generation

> **Status: experimental and disabled by default.** Agent Sandbox is the only supported coding-agent runtime. Fix generation is not part of standard onboarding, only ever starts from an explicit maintainer request, and never merges a pull request.

Aster can investigate a published failure or selected chat finding, attempt a minimal repository patch, and present a draft pull request for explicit human confirmation. Transient severity, low confidence, incomplete remediation, and missing citations or source links do not prevent a maintainer from requesting an attempt. It is the highest-risk optional feature because it writes source code.

Causal-group parents do not produce one combined fix across unrelated causes. A cause-scoped chat can start a proposal tied to that cause's representative failed JUnit test or build-level failure, or the maintainer can open the linked failure. Recovery and resolution status remain visible but do not prevent another investigation while the failed target is still available. Resolving a failure is a separate maintainer acknowledgement, offered per cause so acknowledging one cause leaves the others active.

## Supported workflow

The engine requires a current, unambiguous supported subject and an accessible configured repository, not an implementation-ready diagnosis. The flow is:

1. Resolve the selected subject, authorize the destination, and pin the generation base. Keep analysis-quality concerns as warnings.
2. Start one Agent Sandbox executor against a public source repository at the exact approved revision.
3. Run OpenCode once with Bash, web access, delegation, and external skills disabled.
4. Stage the patch and run the configured exact validators inside the executor.
5. Return the patch and ordered command results to the dashboard.
6. Reconstruct the canonical patch independently and reject unexpected files, paths, identities, commands, or results.
7. Run one warning-only Fix critique when the dashboard AI client is available, then show the exact draft, diff, warnings, validation results, and target.
8. Open the draft pull request only after explicit confirmation of that preview.

A preview is a review artifact, not proof that the patch fixes the failure. Agent Sandbox validators, source verification, Prow CI, and human review are separate gates. The engine never approves or merges the pull request.

## When the coding agent proposes a patch

Every Fix entry point gives the coding agent the same rule:

- First look for a minimal change that makes the failed operation succeed, such as correcting a value, version, path, or repository choice this code made. Verify the causal code path against the repository and cite it by file and line.
- Well-documented external facts, such as which distribution releases a vendor publishes packages for, may support the change. State them in the summary as assumptions with how to validate them.
- Treat the published suggested fix as one candidate, not the default.
- If no resolving change is identifiable, a fail-fast guard is acceptable only when it gives a clearer, earlier error than today. Start the summary with `Guard only:` and say what still needs fixing.
- Make no change when no repository code is causally involved, for example when the fix belongs in another repository or the cause is purely operational. Do not edit unrelated code or skip or weaken the failing test to produce a patch.

The agent ends with a short summary of its change, the causal code path, and its caveats. The draft's **Proposed change** uses that summary, never the published suggested fix. If the summary is missing or fails draft-text validation, it lists the changed files instead.

Fix PR bodies link the failed build to its recorded Prow URL, or list up to five builds for a pattern with a count of the rest. Builds without a recorded URL remain plain text. Verification results, investigation warnings, and any backport suggestion use plain text rather than GitHub callouts. An engine-owned footer repeats the build sources and discloses that an AI coding agent in Aster generated the change. These footer links, verification results, and backport suggestions survive repository-template filling. The footer links Aster to the configured dashboard when available and reminds reviewers to verify the change before merging.

## Warnings and rejections

When the agent returns a patch, quality concerns become warnings on the ready preview and the maintainer decides whether to open the draft:

- failed or timed-out validators, also shown as the verification verdict;
- concrete concerns from the Fix critique, or a warning that critique was unavailable;
- analysis, evidence qualification, and eligibility concerns.

These still reject the attempt because they protect the repository, the credential boundary, or the exact reviewed change: files outside the configured destination, credential exposure, changed Git identity or remotes, a patch that does not reconstruct identically on the pinned generation base, unsafe exact-analysis target drift described below, file deletions or renames (a draft can only add or update files), more files than `max_files`, oversized or malformed executor results, and the remediation safety policy. When the agent makes no change, there is no patch to review; exact-analysis requests report `no_reviewable_patch` with the agent's explanation.

## Exact failure analysis handoff

Authenticated server deployments can turn one exact failed JUnit test or build-level analysis into a fix proposal from the analysis chat. Build-level targets use the job, build, `source: build`, failure name, optional suite/class, and analysis timestamp, without a JUnit file. There is one conversation: ask questions, and any completed nonempty answer can start a proposal, including an uncited or unverified answer. Asking a question never creates a patch, branch, or pull request, and never depends on source verification. The server pins the immutable repository and generation base when the proposal is requested.

After a completed response, **Use this finding in a fix proposal** admits a separate persistent asynchronous preview request. It requires:

- the exact failed test or build-level failure and its published analysis remain available and unchanged, and the run has not passed;
- the selected conversation request completed with a nonempty answer;
- build metadata resolves the exact repository and full tested commit, using `clone-records.json` to identify each repository's commit in multi-repository builds;
- the configured Fix destination matches the analyzed repository;
- for the tested-branch target, the failure revision is the head of that branch or an ancestor of it;
- the target branch still contains the pinned generation base and has not changed a file the patch touches.

The coding agent receives the available failure details, selected answer, retained citations, and optional source hints. It investigates the repository before editing. No citation, source link, named symbol, or previously verified implementation target is required. Missing evidence and unverified claims remain explicitly qualified; they are not relabeled as verified because a proposal was requested.

By default the generation base is the current head of the branch the build reports, so a failure on a release branch is investigated and patched against that release branch. When the repository's default branch differs from that tested branch, the dialog also offers the default branch as the target. That target pins the default branch's current head at admission and skips the ancestry check, since the default branch has its own history. The coding agent is told where the failure was observed and to make no change if the causal logic is absent on the default branch. The PR names both branches and suggests `/cherry-pick <tested branch>` after merge. Request identity, deduplication, and regeneration keep the two targets separate. If Aster cannot determine the exact source commit this build tested, it says a Fix proposal cannot be pinned (`source_revision_unknown`). A tested-branch failure whose commit has diverged from its branch head, or a build that reports no resolvable branch, is rejected before the provider call with a reason code (`source_revision_diverged` or `source_branch_unknown`). These reasons are returned in `X-Analysis-Chat-Reason` and recorded in the server log. Repository-access and generation-base errors remain failures, not analysis warnings.

At admission, the action captures an immutable bounded snapshot of the selected answer, its qualifications, originating test or cause, exact failure target, published analysis and source hints, retained citations, repository, tested branch, target branch, and generation base. A selected answer may reuse citations validated by earlier turns, but later turns do not alter the snapshot. Only retained validated citations are carried as verified artifact evidence. The action generates from that captured context and rechecks current cause membership and content, exact failed test or build evidence, and source/base identity before accepting a generated result and before confirmation. Regenerating the analysis text alone does not revoke an admitted Fix; changed failure evidence or cause content requires a new preview.

Deleting or expiring chat does not revoke an admitted Fix or a Fix request that has already captured its selected answer. Deleting chat before capture prevents a new admission. Use explicit action cancellation to cancel a pending or ready Fix. Existing owner-bound requests can be retrieved without chat; repeated admission reconnects to an existing request instead of silently repinning it. Handoffs must use the current version. Unknown write outcomes retain reconciliation-only recovery and cannot use an incomplete handoff for another write.

Closing the browser or losing the HTTP connection does not cancel an admitted Sandbox. Reopening the dialog restores the initiating operator's request, and repeating the same admission input reconnects instead of creating another Sandbox.

Exact-analysis previews tolerate target-branch advances that leave patched files unchanged. Generation and confirmation compare the pinned base to the current head, rejecting a deleted or rewritten branch or an overlapping file change as `source_changed`. The PR commit stays on the pinned base; Aster does not rebase or rerun validators on the new head. A failed or incomplete GitHub comparison is a source-check failure, not proof of source drift. Pattern and build previews keep their existing pinned-base behavior.

### Preview, warnings, regeneration, and confirmation

The ready preview shows the exact source revision, changed files, canonical diff, validator results, pull-request title and body, and engine-generated warnings. Warnings never authorize a write and cannot be hidden by model prose.

If the reviewer changes the instruction or asks for regeneration, the server creates a replacement request and cancels or supersedes the older active draft. The replacement receives a new identity and must be reviewed from the beginning. The old preview cannot be confirmed after it is superseded.

After a failed, cancelled, or expired exact-analysis request, **Start over** clears that finding's saved request so the maintainer can reselect the target branch, edit the instruction, and generate again in the same dialog. Provider-credential failures also retain **Retry fix preview**. A `no_reviewable_patch` result requires revised feedback to regenerate on the same branch; selecting a different branch starts a fresh request without that feedback requirement.

Confirmation is a separate authenticated POST bound to the owner and exact preview. It rechecks the pattern or admitted failure evidence identity, the action-owned selected finding when applicable, pinned source/base, destination policy, canonical patch, ordered executor results, warning state, and GitHub deduplication marker. Exact-analysis confirmation also rechecks the target branch head. Any bound identity or patch drift fails closed. The dashboard does not rerun target commands during confirmation and never silently regenerates content.

## Command execution and credential boundary

| Process | Responsibility | Credentials and target code |
| --- | --- | --- |
| Dashboard server | Dispatches Agent Sandbox, reconstructs and validates the returned patch, persists the preview, and performs the separately confirmed GitHub write. | May hold OAuth, `BOT_TOKEN`, AI credentials, project data, and the shared data volume. It never executes target build, test, vet, or validation commands. |
| `remote-fixer` image | Provides dashboard binaries and git for canonical patch reconstruction. | Runs inside the normal dashboard Pod boundary. It does not execute target validation commands. |
| Agent Sandbox executor | Clones the public pinned source, runs OpenCode once, stages the patch, runs exact validators, and emits one bounded result. | Receives no GitHub, OAuth, dashboard, or general repository credential. The dedicated provider credential is removed before validator execution. Target code runs only here. |

The executor mounts only bounded workspace and temporary volumes. Its workload ServiceAccount does not automount a token. It does not mount the dashboard PVC. Before validators run, OpenCode state is removed, the provider credential is unset from child environments, and the parent is made non-dumpable. Dashboard processes validate retained command results but do not replay them.

## Fork and direct modes

`ai.fix_prs.fork` selects how the branch reaches GitHub:

- `true`, the default: create or reuse a fork owned by the contributor identity, push the branch there, and open a cross-fork draft PR.
- `false`: push a branch directly to a repository the credential may write and open a same-repository draft PR.

The PR targets the branch the change was generated against: the failure's own branch for an exact-analysis chat finding (so a `release-1.25` failure opens a `release-1.25` pull request) unless the maintainer selects the default branch, and the source repository's default branch otherwise. A branch is never pushed to a repository the configured credential cannot write.

Commits preserve existing file modes from the pinned base tree, including executable scripts. New files use mode `100644`.

## Identity, CLA, and the token

Previews and confirmations use the server-held `BOT_TOKEN`. It must be a PAT for the contributor identity that creates the Fix branch and pull request. It is not the GitHub Actions `GITHUB_TOKEN`.

- In fork mode, a classic PAT needs `public_repo` (or `repo` for private repositories) and `workflow`. Fix branches start from the upstream base, which can include workflow changes the fork lacks, even when the fix edits no workflows. Fine-grained PATs generally work only for repositories owned by the token holder or organization that granted access.
- Direct mode can use a narrowly scoped fine-grained PAT with Contents and Pull requests write permissions on the target repository.
- `author_name` and `author_email` must identify the contributor whose CLA or DCO status will be evaluated by the target repository.
- The engine adds a matching `Signed-off-by` trailer.

Use the narrowest credential that supports the selected mode. Do not put `BOT_TOKEN`, the model provider credential, OAuth credentials, or a general operator PAT inside Agent Sandbox.

Action source checks prefer `SOURCE_INVESTIGATION_GITHUB_TOKEN`, then `GITHUB_READ_TOKEN`, `BOT_TOKEN`, and `GITHUB_TOKEN`. A failed GitHub source check keeps the preview available for confirmation retry; unsafe target drift or changed failure evidence invalidates it.

## Required project configuration

```yaml
ai:
  fix_prs:
    enabled: true
    author_name: "Jane Maintainer"
    author_email: "jane@example.com"
    agent_runtime:
      model_provider:
        credential_mode: direct
        api: responses
        endpoint: https://api.githubcopilot.com/responses
        model: gpt-6-sol
        auth:
          type: bearer
```

Agent Sandbox is the only Fix runtime. The example uses provider-default reasoning effort: pinned OpenCode 1.18.2 ignores explicit effort for GPT-6 on Copilot Responses, so Aster rejects it. Copilot's GPT-5 and later models use Responses except `gpt-5-mini`, which uses Chat Completions. Other Copilot models, including Claude, use Chat Completions; the pinned provider removes the `gpt-5-chat-latest` alias. Non-Copilot endpoints continue to use OpenCode's generic OpenAI-compatible or OpenAI transport.

The runtime defaults to 30 turns, a 10-minute timeout, a 512 KiB output limit, no shell access, and no regeneration. `critique_retries: 0` still runs one warning-only Fix review when the dashboard AI client is available; review concerns or errors never trigger another Agent Sandbox run. `max_turns` reserves one step per validator, then bounds the coding agent's model steps with one additional wrap-up step. The executor stops runs that exceed the step allowance; the elapsed timeout still applies. The repository defaults to `branding.source_repo`, fork mode defaults to true, and `max_files` defaults to 3.

For Agent Sandbox, the configured timeout bounds executor work from process startup and remains capped at 30 minutes. The Pod deadline adds a fixed 2-minute startup allowance and 15 seconds for result finalization. The server run deadline adds another 5 seconds for result retrieval. Cleanup confirmation has a separate 90-second budget, and Sandbox `shutdownTime` covers the run and cleanup budgets. Startup that exceeds the allowance can still prevent a result. Timed-out results retain a bounded, redacted operator summary and any valid partial command results.

When Fix is enabled, asynchronous generation and synchronous Fix previews default to the greater of 10 minutes or the configured agent timeout plus 5 minutes. This headroom allows for work around the Sandbox run, including source checks, cleanup, and patch reconstruction; it does not guarantee completion of unbounded clone or network work. Set `ACTION_TIMEOUT` through `server.extraEnv` to override the generation deadline. An explicit value also controls ordinary action HTTP handlers and must be a positive duration of at least the agent timeout plus 5 minutes when Fix is enabled; a smaller value is rejected at server startup. Without an explicit value, issue, admission, confirmation, cancellation, and escalation handlers keep their existing timeouts. Upstream proxy deadlines can still cut off synchronous HTTP requests.

When `allowed_commands` is omitted, Aster runs only the mandatory staged-diff check. Additional entries use exact argv lists and explicit timeouts, and the final command must remain exactly:

```yaml
- argv: [git, diff, --cached, --check]
  timeout: 1m
```

Shell command strings, generic dispatchers, coding-agent re-entry, empty or multiline arguments, and per-command timeouts above the overall execution bound are rejected. Git is reserved for the final staged-diff check.

Cross-repository remediation requires an explicit `allowed_repositories` entry, path prefixes, destination-specific command allowlist, and fork policy. Defaults from the primary repository are never reused implicitly for a different target.

The complete schema is in [Project configuration](project-configuration.md). Matching chart values, image digests, namespace, RuntimeClass, ServiceAccounts, network policy, storage, and provider Secret belong in the [Kubernetes operator reference](kubernetes-reference.md#agent-sandbox-fix-runtime) and [Kubernetes platform setup](kubernetes-platform.md).

## Agent Sandbox OpenCode executor

Agent Sandbox is installed and upgraded separately from Aster. The Aster chart does not install the controller, CRD, execution namespace, secure RuntimeClass, node infrastructure, provider gateway, provider Secret, or executor image.

The runtime supports direct provider access or an explicit tokenless gateway. Use a dedicated inference-only credential in the execution namespace. The chart references one existing Secret name and key; it does not create, copy, read, or print the value. Admission pins the credential shape and rejects `envFrom`, Secret volumes, projected tokens, extra credentials, arbitrary environment entries, and unexpected images or identities.

The provider protocol and OpenCode compatibility details are in [AI providers](ai-providers.md#agent-sandbox-provider-compatibility). The executor selects OpenCode's Copilot SDK for Copilot endpoints, with the dedicated credential passed only through the fixed environment variable. It disables the title agent and background model catalog fetch. TLS, egress, RuntimeClass, and provider-neutral isolation requirements are in [Kubernetes platform setup](kubernetes-platform.md#secure-runtime-contract).

After OpenCode returns, the executor rejects credential leakage in process output, structured results, patches, changed files, command output, and failure data. It then runs validators with a credential-free environment and the image's local Go toolchain. Complete, authentic failed or timed-out validator results produce a warned preview; missing executables, malformed results, and integrity failures block a preview.

When the model provider rejects a Fix request, the failed request names the failure category and shows a bounded, redacted operator diagnostic with the HTTP status, provider error code, configured Secret reference, endpoint host and path, model, and provider message. Authentication failures remain `provider_credential`; other non-transient 4xx rejections are `provider_request`. Coding-agent and runtime failures show a bounded reason. The server logs the operator diagnostic for failed requests.

The generic executor image contains the pinned Go toolchain, OpenCode, git, and CA certificates. It does not promise repository-specific tools. Configure only commands available in the selected immutable image. Public repositories are required because no Git credential enters the Sandbox.

## Freshness, deduplication, and private state

Fix generation requires a currently addressable published subject. A retained pattern can start a manual attempt while its supporting evidence remains available. An individual failed build can use the preview and confirmation flow without accepted critique or preidentified source files. Issue drafting retains its separate, stricter remediation eligibility rules.

Hidden GitHub markers and private state deduplicate the same job and root-cause identity. Different causes on the same job may produce separate drafts. Persistent preview and audit files are private operational state and are never served under `/data/*` or published to Pages.

## Known limitations

- A draft preview is not a correctness proof and never bypasses repository CI or human review. Aster does not follow a merged pull request through Prow and never certifies that it fixed the original failure. Later dashboard runs are fresh observations, not proof attributed to a particular pull request.
- Local state plus GitHub search deduplication is not atomic across overlapping requests. Serialize concurrent writers.
- A newly created fork may not be immediately ready for the first push. A later explicit retry can succeed after GitHub finishes populating it.
- The generic executor supports only tools present in its immutable image.
- Provider and validator failures are terminal for that request. There is no iterative test-feedback repair loop.
