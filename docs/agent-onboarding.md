# Agent-driven setup and diagnostic authoring

The repository ships two portable Agent Skills for compatible LLM CLIs:

- [`setup-prow-ai-consumer`](../.agents/skills/setup-prow-ai-consumer/SKILL.md)
  creates or updates a validated consumer through the engine CLI.
- [`author-prow-ai-diagnostics`](../.agents/skills/author-prow-ai-diagnostics/SKILL.md)
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
npx --yes skills@latest add willie-yao/prow-ai-dashboard \
  --skill setup-prow-ai-consumer author-prow-ai-diagnostics \
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
npx --yes skills@latest add willie-yao/prow-ai-dashboard \
  --skill setup-prow-ai-consumer author-prow-ai-diagnostics \
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
cp -R /path/to/prow-ai-dashboard/.agents/skills/setup-prow-ai-consumer .agents/skills/
cp -R /path/to/prow-ai-dashboard/.agents/skills/author-prow-ai-diagnostics .agents/skills/
```

Personal scope:

```bash
mkdir -p ~/.agents/skills
cp -R /path/to/prow-ai-dashboard/.agents/skills/setup-prow-ai-consumer ~/.agents/skills/
cp -R /path/to/prow-ai-dashboard/.agents/skills/author-prow-ai-diagnostics ~/.agents/skills/
```

Use the client-specific personal directory only when the client does not support
portable `.agents/skills` locations.

## Set up a dashboard with an agent

Invoke the initial setup skill with a concrete source repository or URL. The
agent should use supplied values directly, derive the project slug, run discovery
before asking about selectors, and use the discovery-suggested consumer identity
when one is available:

```text
Use $setup-prow-ai-consumer to create a Pages consumer for
https://github.com/kubernetes-sigs/kueue. Store it in a timestamped Codex
workspace under ~/.codex/deployments/prow-ai-dashboard, exclude presubmits,
and keep deployed AI enabled. Do not ask me to clone the source repository.
```

```text
Use $setup-prow-ai-consumer for
https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver and
put the consumer files in the current directory.
```

```text
Use $setup-prow-ai-consumer to create a separate Kubernetes consumer checkout
for kubernetes-sigs/secrets-store-csi-driver.
```

The agent should not ask again for a source URL, project slug, job name,
deployment mode, or workspace path already supplied in the request. It should
ask only when the source is absent or discovery leaves a materially ambiguous
choice. Running the skill from the dashboard engine checkout does not make the
engine repository the source project.

When exact jobs are named, the agent should use bucket discovery with repeated
`-exact-job` flags rather than accepting a shared TestGrid dashboard containing
unrelated jobs. For a Codex evaluation workspace it also records exact engine
identity and writes `manifest/locations.json`,
`manifest/consumer-files.sha256`, and `reports/setup-summary.md`.

The skill should also trigger for requests such as:

- “Set up a prow-ai-dashboard consumer for this project.”
- “Create a dashboard consumer repo for this repository.”
- “Run onboarding without the interactive wizard.”
- “Add the consumer files to this repository.”

### What the setup skill can prepare

- Consumer files in the current directory, a subdirectory, an existing checkout,
  or a separate repository directory.
- GitHub Pages or Kubernetes deployment files.
- TestGrid or bucket-based job discovery.
- Optional presubmit inclusion.
- An agent-authored project prompt using the generated handoff and engine-owned
  prompt-generation skill.
- A final read-only doctor check.

Git initialization, GitHub repository creation, pushes, pull requests, Pages
configuration, Secret writes, Helm installation, and deployment remain separate
confirmation-gated actions.

### Expected setup workflow

The agent should:

1. Determine the source and intended consumer repository identities.
2. Run `fetcher onboard discover -json`.
3. Ask for unresolved placement, deployment, and discovery decisions.
4. Run a complete non-interactive dry run with `-prompt-mode handoff` and
   `-plan-out`.
5. Present every planned create or replace action and the plan digest.
6. Apply the saved artifact with `-apply-plan` and `-plan-digest` after
   confirmation.
7. Follow `PROMPT_HANDOFF.md` and the generated
   `system-prompt-generation/SKILL.md` to complete `prompts/system.md`.
8. Run `fetcher onboard doctor`.
9. Report remaining checklist and deployment work.

For existing generated files, the first dry run reports conflicts. The agent
must ask before rerunning with `-update-existing`, then review the complete
replacement plan. Onboarding's direct `-open-pr` mode is intentionally not used
because prompt completion and doctor require local files. The temporary plan
artifact must stay outside the consumer destination.

The skill must use `fetcher onboard` rather than hand-writing `project.yaml`,
workflows, Helm values, or deployment guides. This keeps agent-driven setup on
the same discovery, validation, credential, path, and generated-file contracts
as the wizard.

## Improve diagnostics after setup

After the consumer passes doctor, invoke:

```text
Use $author-prow-ai-diagnostics to investigate representative historical
failures for this consumer, improve prompts/system.md, and propose recipes only
under proposals/skills without activating them.
```

The diagnostic-authoring skill:

- Pins engine, source, test-infra, job, build, prompt, and recipe identities.
- Uses a representative historical failure corpus rather than tuning to one
  selected failure.
- Preserves the required nine-section project prompt contract.
- Proposes recipes only for repeated prompt-only misses.
- Validates trigger polarity, evidence groups, collisions, and held-out cases.
- Writes proposals under `proposals/skills/` and reports under `reports/`.
- Can abstain when the evidence does not justify a prompt or recipe change.
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
  setup-prow-ai-consumer author-prow-ai-diagnostics \
  --global \
  --yes
```

Review upstream skill changes before using a new revision in automated or
write-enabled workflows. For manual copies, replace the complete skill
directory from a newer trusted engine checkout.
