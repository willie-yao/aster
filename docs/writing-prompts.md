# Writing a project AI prompt

Every consumer with AI enabled must provide a non-empty `prompts/system.md` next to `project.yaml`. The file is a project-specific diagnostic runbook. It should tell the analyzer how to localize failures, which artifacts support each claim, and which details remain unknown.

The engine fails at startup when AI is enabled and the prompt is missing or blank. Use [`configs/example/prompts/system.md`](../configs/example/prompts/system.md) as the minimal current example.

## Prompt composition

The engine composes the system prompt in this order:

```text
engine BasePrompt
consumer prompts/system.md, verbatim
engine ResponseFormatFooter
engine agentic tool guidance
```

- [`BasePrompt`](../backend/internal/ai/baseprompt.go) provides universal Prow artifact entry points and a generic triage order.
- The consumer prompt supplies project-specific architecture, artifact, and failure knowledge. The engine does not edit it.
- [`ResponseFormatFooter`](../backend/internal/ai/responseformat.go) owns the structured response contract. Do not redeclare the output schema.
- Agentic tool documentation is engine-owned. Do not describe tool names, budgets, or provider protocol in the consumer prompt.

Onboarding `handoff` mode writes a TODO prompt plus a portable prompt-authoring skill without calling a model. `todo-template` writes only the TODO prompt. Both paths are credential-free. The result is a source-based draft that requires human review and later validation against historical failures.

## Required runbook headings

Keep these headings in this order:

```markdown
## Architecture
## Diagnostic lifecycle
## Test and job flavors
## Artifact layout
## Common failure patterns
## Transient classification
## Triage order
## Relevant source repositories
## Unresolved details
```

### Architecture

Name the components involved in tested behavior and the dependencies between them. Include only relationships established by current source or operator knowledge.

### Diagnostic lifecycle

Describe the expected sequence from job start through setup, test execution, cleanup, and artifact upload. This gives the analyzer a causal timeline.

### Test and job flavors

Explain how periodic, presubmit, upgrade, conformance, unit, or project-specific flavors differ. State when JUnit is absent and only build-level analysis is possible.

### Artifact layout

Map high-value evidence to its real location. Include important logs, JUnit files, resource dumps, and flavor-specific subtrees. Prefer paths and stable patterns over prose.

### Common failure patterns

For each recurring class, provide the exact signal, the evidence that confirms it, common downstream noise, and the narrow remediation boundary. Do not turn a hypothesis into a fact.

### Transient classification

List known transient signatures and the evidence required to call them transient. Distinguish infrastructure retries from deterministic product or test failures.

### Triage order

Give a short artifact-first sequence. Start with the test result and build metadata, then move to the earliest causal log or resource evidence. Avoid long checklists that encourage reading everything.

### Relevant source repositories

List only repositories that artifact or source evidence can use for grounded `relevant_files`. Prefer GitHub `owner/name` form. Do not invent repository identities.

### Unresolved details

Record important paths, flavors, dependencies, or failure boundaries that the available sources do not establish. Keep explicit maintainer TODOs instead of filling gaps with generic assumptions.

## Artifact-first guidance

- Quote real failure signatures and name the artifact that contains them.
- Separate initiating causes from later cleanup or timeout noise.
- Require evidence before classifying a failure as transient.
- Keep remediation concrete and bounded to what the evidence supports.
- Use source paths only when the pinned repository evidence justifies them.
- Prefer concise tables and lists to repeated narrative.

The analyzer can read the supplied Prow artifact tree and, when configured, read-only pinned source. Kubernetes-shaped tools inspect resources already captured in artifacts; they do not connect to a live cluster. The analyzer has no portal, SSH, arbitrary shell, browser, or local CLI access. Never describe an unavailable manual check as evidence already collected.

## Review and iteration

Review the initial draft for unsupported architecture claims, stale paths, missing job flavors, and unclosed placeholders. Then run `$author-aster-diagnostics` against a representative historical corpus. That skill may improve the prompt and propose inactive evidence recipes, but it must not activate recipes or tune only to one favorable case.

Editing `prompts/system.md` affects new analyses. Existing reusable cache entries keep the `prompt_hash` provenance that produced them. Set `AI_CACHE_GENERATION` to a new non-empty value when a prompt rewrite requires an intentional full rebaseline. Returning to a previous generation reuses its unexpired entries. Use destructive cache clearing only for emergency recovery.

For adjacent contracts, see:

- [Agentic analysis](agentic.md) for tools, quality gates, and cache acceptance.
- [Diagnostic skills](skills.md) for evidence recipes.
- [Project configuration](project-configuration.md) for exact fields.
