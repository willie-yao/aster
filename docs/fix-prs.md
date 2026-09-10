# Experimental Fix PR generation

> **Status: experimental and disabled by default.** Agent Sandbox is the only supported coding-agent runtime. Fix generation is not part of standard onboarding, only ever starts from an explicit maintainer request, and never merges a pull request.

Aster can investigate a published failure or selected chat finding, attempt a minimal repository patch, and present a draft pull request for explicit human confirmation. Transient severity, low confidence, incomplete remediation, and missing citations or source links do not prevent a maintainer from requesting an attempt. It is the highest-risk optional feature because it writes source code.

Causal-group parents do not produce one combined fix across unrelated causes. A cause-scoped chat can start a proposal tied to that cause's representative exact JUnit failure, or the maintainer can open the linked failure. Recovery and resolution status remain visible but do not prevent another investigation while the failed target is still available. Resolving a failure is a separate maintainer acknowledgement, offered per cause so acknowledging one cause leaves the others active.

## Supported workflow

The engine requires a current, unambiguous supported subject and an accessible configured repository, not an implementation-ready diagnosis. The flow is:

1. Resolve the selected subject, authorize the destination, and pin the generation base. Keep analysis-quality concerns as warnings.
2. Start one Agent Sandbox executor against a public source repository at the exact approved revision.
3. Run OpenCode once with Bash, web access, delegation, and external skills disabled.
4. Stage the patch and run the configured exact validators inside the executor.
5. Return the patch and ordered command results to the dashboard.
6. Reconstruct the canonical patch independently and reject unexpected files, paths, identities, commands, or results.
7. Show the exact draft, diff, warnings, validation results, and target.
8. Open the draft pull request only after explicit confirmation of that preview.

A preview is a review artifact, not proof that the patch fixes the failure. Agent Sandbox validators, source verification, Prow CI, and human review are separate gates. The engine never approves or merges the pull request.

## Exact JUnit analysis handoff

Authenticated server deployments can turn one exact failed JUnit analysis into a fix proposal from the analysis chat. There is one conversation: ask questions, and any completed nonempty answer can start a proposal, including an uncited or unverified answer. Asking a question never creates a patch, branch, or pull request, and never depends on source verification. The server pins the immutable repository and generation base when the proposal is requested.

After a completed response, **Use this finding in a fix proposal** admits a separate persistent asynchronous preview request. It requires:

- the exact failed JUnit case and its published analysis remain available and unchanged;
- the selected conversation request completed with a nonempty answer;
- build metadata resolves the exact repository and full commit;
- the configured Fix destination matches the analyzed repository;
- the failure revision is the head of its own tested branch or an ancestor of that head;
- the pinned generation base remains current through preview generation and confirmation.

The coding agent receives the available failure details, selected answer, retained citations, and optional source hints. It investigates the repository before editing. No citation, source link, named symbol, or previously verified implementation target is required. Missing evidence and unverified claims remain explicitly qualified; they are not relabeled as verified because a proposal was requested.

The generation base is resolved for the branch the build reports, so a failure on a release branch is investigated and patched against that release branch. A failure whose commit has diverged from its branch head, or whose build reports no resolvable branch, is rejected before the provider call with a reason code (`source_revision_diverged` or `source_branch_unknown`) returned in `X-Analysis-Chat-Reason` and recorded in the server log. Repository-access and generation-base errors remain failures, not analysis warnings.

Evidence is conversation-scoped. A later answer may reuse evidence validated by an earlier turn in the same conversation, but turns after the promoted answer do not alter an admitted request. Only retained validated citations are carried as verified artifact evidence. Any change to the bound analysis, selected answer or its qualification, source revision, or generation base requires a new session or preview.

Closing the browser or losing the HTTP connection does not cancel an admitted Sandbox. Reopening the dialog restores the initiating operator's request, and repeating the same admission input reconnects instead of creating another Sandbox.

### Preview, warnings, regeneration, and confirmation

The ready preview shows the exact source revision, changed files, canonical diff, validator results, pull-request title and body, and engine-generated warnings. Warnings never authorize a write and cannot be hidden by model prose.

If the reviewer changes the instruction or asks for regeneration, the server creates a replacement request and cancels or supersedes the older active draft. The replacement receives a new identity and must be reviewed from the beginning. The old preview cannot be confirmed after it is superseded.

Confirmation is a separate authenticated POST bound to the owner and exact preview. It rechecks the analysis or pattern identity, selected chat context when applicable, pinned source/base, destination policy, canonical patch, ordered executor results, warning state, and GitHub deduplication marker. Exact-JUnit confirmation also rechecks the tested branch head. Any bound identity or patch drift fails closed. The dashboard does not rerun target commands during confirmation and never silently regenerates content.

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

The PR targets the branch the change was generated against: the failure's own branch for an exact JUnit analysis (so a `release-1.25` failure opens a `release-1.25` pull request), and the source repository's default branch otherwise. A branch is never pushed to a repository the configured credential cannot write.

## Identity, CLA, and the token

Previews and confirmations use the server-held `BOT_TOKEN`. It must be a PAT for the contributor identity that creates the Fix branch and pull request. It is not the GitHub Actions `GITHUB_TOKEN`.

- Cross-fork public contribution commonly requires a classic PAT with `public_repo`; private repositories require the corresponding repository access. Fine-grained PATs generally work only for repositories owned by the token holder or organization that granted access.
- Direct mode can use a narrowly scoped fine-grained PAT with Contents and Pull requests write permissions on the target repository.
- `author_name` and `author_email` must identify the contributor whose CLA or DCO status will be evaluated by the target repository.
- The engine adds a matching `Signed-off-by` trailer.

Use the narrowest credential that supports the selected mode. Do not put `BOT_TOKEN`, the model provider credential, OAuth credentials, or a general operator PAT inside Agent Sandbox.

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
        api: chat_completions
        endpoint: https://provider.example/v1/chat/completions
        model: provider-model-id
        auth:
          type: bearer
```

Agent Sandbox is the only Fix runtime. It defaults to 30 turns, a 10-minute timeout, a 512 KiB output limit, no shell access, and no model critique retry. The repository defaults to `branding.source_repo`, fork mode defaults to true, and `max_files` defaults to 3.

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

The provider protocol and OpenCode compatibility details are in [AI providers](ai-providers.md#agent-sandbox-provider-compatibility). TLS, egress, RuntimeClass, and provider-neutral isolation requirements are in [Kubernetes platform setup](kubernetes-platform.md#secure-runtime-contract).

After OpenCode returns, the executor rejects credential leakage in process output, structured results, patches, changed files, command output, and failure data. It then runs validators with a credential-free environment. A validator failure, missing executable, unexpected command result, or invalid final diff produces no actionable preview.

The generic executor image contains the pinned Go toolchain, OpenCode, git, and CA certificates. It does not promise repository-specific tools. Configure only commands available in the selected immutable image. Public repositories are required because no Git credential enters the Sandbox.

## Freshness, deduplication, and private state

Fix generation requires a currently addressable published subject. A retained pattern can start a manual attempt while its supporting evidence remains available. An individual failed build can use the preview and confirmation flow without accepted critique or preidentified source files. Issue drafting retains its separate, stricter remediation eligibility rules.

Hidden GitHub markers and private state deduplicate the same job and root-cause identity. Different causes on the same job may produce separate drafts. Persistent preview and audit files are private operational state and are never served under `/data/*` or published to Pages.

## Known limitations

- A draft preview is not a correctness proof and never bypasses repository CI or human review. Aster does not follow a merged pull request through Prow and never certifies that it fixed the original failure. Later dashboard runs are fresh observations, not proof attributed to a particular pull request.
- Edited files are committed as regular files, so a reviewer must notice an unintended executable-bit change.
- Local state plus GitHub search deduplication is not atomic across overlapping requests. Serialize concurrent writers.
- A newly created fork may not be immediately ready for the first push. A later explicit retry can succeed after GitHub finishes populating it.
- The generic executor supports only tools present in its immutable image.
- Provider and validator failures are terminal for that request. There is no iterative test-feedback repair loop.
