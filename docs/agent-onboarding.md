# Agent-driven setup and diagnostic authoring

The repository ships two portable Agent Skills for compatible LLM CLIs:

- [`setup-aster-consumer`](../.agents/skills/setup-aster-consumer/SKILL.md)
  creates or updates a validated consumer through the engine CLI.
- [`author-aster-diagnostics`](../.agents/skills/author-aster-diagnostics/SKILL.md)
  investigates a valid consumer, improves its project prompt, proposes bounded
  diagnostic recipes, and benchmarks held-out failures without activating the
  recipes.

Use the setup skill first. Use the diagnostic-authoring skill only after the
consumer passes `onboard doctor`.

The skills do not replace engine behavior. They gather decisions and coordinate
the existing discovery, dry-run, apply, prompt, validation, and benchmark
contracts instead of maintaining separate scaffold or recipe implementations.

## Install with the Skills CLI

The recommended installation method is the cross-agent
[Skills CLI](https://github.com/sozercan/skills). It discovers both skills from
this repository and manages project or personal installation without manually
copying their supporting files.

Install both skills for Codex at personal scope:

```bash
npx --yes skills@latest add willie-yao/aster \
  --skill setup-aster-consumer author-aster-diagnostics \
  --agent codex \
  --global \
  --yes
```

Verify the installation:

```bash
npx --yes skills@latest list --global --agent codex
```

For project scope, run the same command from the intended workspace without
`--global`:

```bash
npx --yes skills@latest add willie-yao/aster \
  --skill setup-aster-consumer author-aster-diagnostics \
  --agent codex \
  --yes
```

Omit `--agent codex` to let the CLI select another detected Agent Skills client.
The dashboard engine does not depend on the Skills CLI at runtime. It is only an
installation and update convenience for the LLM CLI.

Start a new agent session after installation. Restart the client if it does not
refresh installed skills automatically. Codex skill discovery and locations are
documented in the [Codex Skills guide](https://developers.openai.com/codex/skills/).

## Manual installation fallback

If the Skills CLI is unavailable, copy each complete directory from a cloned
engine checkout. Do not copy only `SKILL.md`, because the skills include UI
metadata and references.

Project scope:

```bash
mkdir -p .agents/skills
cp -R /path/to/aster/.agents/skills/setup-aster-consumer .agents/skills/
cp -R /path/to/aster/.agents/skills/author-aster-diagnostics .agents/skills/
```

Personal scope:

```bash
mkdir -p ~/.agents/skills
cp -R /path/to/aster/.agents/skills/setup-aster-consumer ~/.agents/skills/
cp -R /path/to/aster/.agents/skills/author-aster-diagnostics ~/.agents/skills/
```

Use the client-specific personal directory only when the client does not support
portable `.agents/skills` locations.

## Set up a dashboard with an agent

Invoke the initial setup skill with a concrete source repository or URL. The
agent should use supplied values directly, derive the project slug, run discovery
before asking about selectors, and use the discovery-suggested consumer identity
when one is available:

```text
Use $setup-aster-consumer to create a Pages consumer for
https://github.com/kubernetes-sigs/kueue. Store it in a timestamped Codex
workspace under ~/.codex/deployments/aster, exclude presubmits,
and keep deployed AI enabled. Do not ask me to clone the source repository.
```

```text
Use $setup-aster-consumer for
https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver and
put the consumer files in the current directory.
```

```text
Use $setup-aster-consumer to create a separate Kubernetes consumer checkout
for kubernetes-sigs/secrets-store-csi-driver.
```

The agent should not ask again for a source URL, project slug, job name,
deployment mode, or workspace path already supplied in the request. It should
ask only when the source is absent or discovery leaves a materially ambiguous
choice. Running the skill from the dashboard engine checkout does not make the
engine repository the source project.

When exact jobs are named, the agent should use bucket discovery with repeated
`-exact-job` flags rather than accepting a shared TestGrid dashboard containing
unrelated jobs. For a Codex evaluation workspace it records exact engine, source, catalog, and
job identities and writes canonical `manifest/apply-result.json` and
`manifest/setup-handoff.json` outputs. Compatibility summaries may also include
`manifest/locations.json`, `manifest/consumer-files.sha256`, and
`reports/setup-summary.md`.

The skill should also trigger for requests such as:

- “Set up an Aster consumer for this project.”
- “Create a dashboard consumer repo for this repository.”
- “Run onboarding without the interactive wizard.”
- “Add the consumer files to this repository.”

### What the setup skill can prepare

- Consumer files in the current directory, a subdirectory, an existing checkout,
  or a separate repository directory.
- GitHub Pages or Kubernetes deployment files.
- TestGrid or bucket-based job discovery.
- Optional presubmit inclusion.
- A source-only prompt baseline or preservation of an existing consumer prompt.
- A read-only artifact-usability smoke check and final doctor report, including
  a warning when sampled builds have no JUnit and only build-level analysis may
  be available.
- A validated machine-readable handoff with first-class artifact location and
  test-infra identity for `$author-aster-diagnostics`.

Git initialization, GitHub repository creation, pushes, pull requests, Pages
configuration, Secret writes, Helm installation, and deployment remain separate
confirmation-gated actions.

### Expected setup workflow

The agent should:

1. Determine the source and intended consumer repository identities.
2. Run `aster onboard discover -json`.
3. Select Pages or Kubernetes from artifact privacy, provider reachability,
   authentication, persistent state, admin actions, and cluster-local endpoints.
4. Run a complete non-interactive dry run with `-prompt-mode handoff`,
   `-artifact-access`, repeated `-deployment-reason`, and `-plan-out`.
5. Present exact engine/source/catalog/job pins plus every create, replace, or
   preserve action and the plan digest.
6. Apply the saved artifact with `-apply-plan`, `-plan-digest`, `-result-out`,
   `-handoff-out`, and `-artifact-smoke-builds` after confirmation.
7. Validate `manifest/setup-handoff.json` with the setup skill's bundled script.
8. Pass the validated handoff to `$author-aster-diagnostics`.
9. Report remaining checklist and deployment work.

For an existing consumer, the first dry run reports conflicts. The agent must
ask before rerunning with `-update-existing`. Update mode replaces only
engine-generated files and preserves `prompts/system.md` plus existing
`skills/*.yaml` or `skills/*.yml`. Replacing the prompt requires a separate
existing-versus-candidate diff, explicit approval, and a new plan with
`-replace-consumer-owned`. Existing skills are never replaced by setup.

Onboarding's direct `-open-pr` mode is intentionally not used because local
doctor, artifact smoke, and handoff validation require local files. The plan and
result files stay outside the consumer destination.

The skill must use `aster onboard` rather than hand-writing `project.yaml`,
workflows, Helm values, or deployment guides. This keeps agent-driven setup on
the same discovery, validation, credential, path, preservation, and hashing
contracts as the wizard.

## Improve diagnostics after setup

After the consumer passes doctor, invoke:

```text
Use $author-aster-diagnostics to investigate representative historical
failures for this consumer, improve prompts/system.md, and propose recipes only
under proposals/skills without activating them.
```

The diagnostic-authoring skill:

- Validates and consumes `setup-handoff.json` without repeating pinned setup
  discovery.
- Pins engine, source, test-infra, artifact location, job, build, prompt, and
  recipe identities.
- Uses a representative historical failure corpus rather than tuning to one
  selected failure.
- Preserves the required nine-section project prompt contract.
- Proposes recipes only for repeated prompt-only misses.
- Validates trigger polarity, evidence groups, collisions, and held-out cases.
- Writes proposals under `proposals/skills/` and reports under `reports/`.
- Can abstain when the evidence does not justify a prompt or recipe change.
- Records a non-Git consumer with null commit and
  `commit_status: not_applicable`, not an all-zero placeholder.
- Uses a detached validation worktree or a copy with `.git` fully excluded, so a
  validation commit cannot modify the pinned engine branch.
- Labels remote GCS or HTTP blind access `self_reported` unless evidence was
  first copied into a wrapper-controlled local tree.
- Never promotes proposals into active `skills/` without later explicit
  approval.

Keep held-out diagnoses, benchmark scoring rules, prior dashboard answers, and
manual intervention recipes out of the authoring session until its outputs are
frozen.

## Update installed skills

List the currently linked global skills:

```bash
npx --yes skills@latest list --global --agent codex
```

Update these global installations from their recorded source:

```bash
npx --yes skills@latest update \
  setup-aster-consumer author-aster-diagnostics \
  --global \
  --yes
```

Review upstream skill changes before using a new revision in automated or
write-enabled workflows. For manual copies, replace the complete skill
directory from a newer trusted engine checkout.
