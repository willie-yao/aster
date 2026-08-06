# Agent-driven consumer setup

The repository ships a portable Agent Skill that lets an agent set up a
`prow-ai-dashboard` consumer without walking through the interactive wizard:

[`setup-prow-ai-consumer`](../.agents/skills/setup-prow-ai-consumer/SKILL.md)

The skill does not replace onboarding logic. It gathers decisions
conversationally, runs read-only discovery, uses the real `fetcher onboard`
dry-run and apply paths, completes the generated prompt handoff, and runs the
read-only doctor.

## What the skill can set up

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

## Install the complete skill directory

Copy the complete `setup-prow-ai-consumer` directory. Do not copy only
`SKILL.md`, because the skill includes UI metadata and decision references.

Set the source path to a cloned `prow-ai-dashboard` checkout:

```bash
SKILL_SOURCE=/path/to/prow-ai-dashboard/.agents/skills/setup-prow-ai-consumer
```

### OpenCode

Project scope:

```bash
mkdir -p .agents/skills
cp -R "$SKILL_SOURCE" .agents/skills/
```

Personal scope:

```bash
mkdir -p ~/.agents/skills
cp -R "$SKILL_SOURCE" ~/.agents/skills/
```

OpenCode discovers skills from `.agents/skills`, `.opencode/skills`, and
compatible Claude skill directories. See the
[OpenCode skills documentation](https://opencode.ai/docs/skills/).

### Claude Code

Project scope:

```bash
mkdir -p .claude/skills
cp -R "$SKILL_SOURCE" .claude/skills/
```

Personal scope:

```bash
mkdir -p ~/.claude/skills
cp -R "$SKILL_SOURCE" ~/.claude/skills/
```

See the [Claude Code Agent Skills documentation](https://code.claude.com/docs/en/skills)
for managed, personal, and project skill locations.

### Codex

Personal CLI installation:

```bash
CODEX_SKILLS_DIR="${CODEX_HOME:-$HOME/.codex}/skills"
mkdir -p "$CODEX_SKILLS_DIR"
cp -R "$SKILL_SOURCE" "$CODEX_SKILLS_DIR/"
```

The skill can also be imported from a GitHub checkout through supported Codex
Skills surfaces. OpenAI Skills follow the portable Agent Skills standard; see
[Skills in ChatGPT](https://help.openai.com/en/articles/20001066).

### Other Agent Skills clients

Copy the complete directory into the client's project or personal skills
location. The canonical skill uses standard `SKILL.md` frontmatter and keeps
supporting material under `references/`.

## Invoke the skill

Examples:

```text
Use $setup-prow-ai-consumer to set up a Pages consumer for this repository.
```

```text
Use $setup-prow-ai-consumer to put the consumer files in the current directory.
```

```text
Use $setup-prow-ai-consumer to create a separate Kubernetes consumer checkout.
```

The skill should trigger naturally for requests such as:

- “Set up a prow-ai-dashboard consumer for this project.”
- “Create a dashboard consumer repo for CAPZ.”
- “Run onboarding without the interactive wizard.”
- “Add the consumer files to this repository.”

## Expected workflow

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
must ask before rerunning with `-update-existing`, then review that full plan.
Onboarding's direct `-open-pr` mode is intentionally not used because prompt
completion and doctor require local files. The temporary plan artifact must stay
outside the consumer destination. Apply also rejects a replacement file edited
after review.

The skill must use `fetcher onboard` rather than hand-writing `project.yaml`,
workflows, Helm values, or deployment guides. This keeps agent-driven setup on
the same discovery, validation, credential, path, and generated-file contracts
as the wizard.

## Update the installed skill

Skills are ordinary directories. To update an installed copy, remove or rename
the old copy and copy the complete directory from the newer engine checkout.
Review the skill diff before using a new revision in an automated workflow.
