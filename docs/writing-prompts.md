# Writing a project AI prompt

Every consumer of [prow-ai-dashboard][engine] must ship a `prompts/system.md`
alongside its `project.yaml`. This file is what makes the AI summaries useful
for your project. It should be an artifact-backed diagnostic runbook that tells
the model how to localize a failure, which evidence proves each conclusion, and
which details remain unknown.

The engine hard-errors at startup if `prompts/system.md` is missing or
whitespace-only when `-ai` is enabled. There is no "default project prompt";
generic AI analysis on Prow logs without project context produces hallucinations
faster than it produces signal.

[engine]: https://github.com/willie-yao/prow-ai-dashboard

## How the prompt is composed

At fetcher startup the engine assembles the final system prompt sent to the
chat completions API as:

```
<engine BasePrompt>

## Project-specific knowledge

<your prompts/system.md, verbatim>

<engine ResponseFormatFooter>
```

- **`BasePrompt`** ([baseprompt.go][baseprompt]) is a ~150-word universal
  preamble: GCS artifact layout, the `build-log.txt` / `started.json` /
  `junit_*.xml` entry points, and a generic triage order. It is the same for
  every consumer.
- **Your `prompts/system.md`** is the variable part. It is appended verbatim
  with no editorial filtering. What you write is what the model sees.
- **`ResponseFormatFooter`** ([responseformat.go][footer]) pins the JSON
  schema the Go code unmarshals (`summary`, `is_transient`, `root_cause`,
  `severity`, `suggested_fix`, `relevant_files`). Do NOT redeclare it in your
  addendum; if you do, you risk the model returning a shape the engine cannot
  parse.
- **Agentic mode addendum.** When the engine analyzes a failure under
  [agentic mode](agentic.md), it also appends an engine-owned tool-docs
  section to the assembled prompt that documents the `list_artifacts` /
  `read_artifact` / `tail_artifact` / `grep_artifact` tools and the
  per-failure byte budgets. Your `prompts/system.md` never sees this
  section; do not document the tools yourself.

[baseprompt]: ../backend/internal/ai/baseprompt.go
[footer]: ../backend/internal/ai/responseformat.go

## How onboarding drafts the prompt

Guided onboarding can draft this file from a bounded evidence corpus after you
confirm that repository content may be sent to the selected provider. The input
combines:

- Markdown documentation.
- Relevant Go, YAML, and shell excerpts from one commit resolved from the
  default branch.
- Matched Prow job names, types, configuration files, repositories, branches or
  refs, and TestGrid annotations already found during planning.

Selection is deterministic. It uses at most 10 source files or excerpts, at most
20,000 bytes from one source, and at most 80,000 source bytes total. Large files
are excerpted around diagnostic terms and retain line ranges. Vendored,
generated, unsupported binary, `node_modules`, and `.github` paths are excluded.
Documentation references and Prow configuration paths can raise an exact source
path's rank. The job section is separately limited to 100 jobs and 40,000
bytes, with an omitted-count summary when more jobs match.

Eligible source files up to 1 MiB are scanned before the line-ranged excerpt is
selected. A truncated recursive Git tree is rejected rather than presented as a
complete deterministic corpus.

Onboarding does not clone the repository, execute repository code, use GitHub
code search, or send the whole repository. Repository text and job metadata are
untrusted evidence. Documentation references may influence deterministic ranking
of eligible files in the pinned snapshot, but cannot trigger arbitrary URLs,
commands, provider-time retrieval, or secret access. The draft remains a
starting point that requires human review.

Generation uses two structured completion stages within a five-minute total timeout by default. `fetcher onboard --prompt-timeout` can raise the total budget for a slow provider:

1. Extract an evidence object whose claims retain supplied source paths and line
   ranges.
2. Validate it deterministically, then ask for one complete structured revision
   against the quality rubric.
3. Validate the revision with the same rules and render Markdown deterministically.

Each stage uses the engine's existing structured transport: native JSON schema,
then a forced function call, then bounded plain-JSON extraction when the provider
rejects the earlier protocol. This is at most three transport attempts per stage
and six total; validation failure never triggers an unbounded retry loop.

If the first extraction is invalid, onboarding writes the reviewable stub. If
the revision fails, onboarding renders the first validated evidence object. It
never publishes an unvalidated object or asks a third free-form call to format
Markdown. Source references remain internal to generation unless their content
is useful in the runbook.

A capable model does not replace source quality. Improve repository diagnostics,
artifact documentation, or job metadata, then rerun onboarding when important
details remain unresolved. The generated file is always a draft requiring human
review.

## Required runbook sections

The onboarding generator requires these sections in this order. Keeping the
same structure in hand-written prompts makes review and iteration easier.

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

Describe only component, resource, and repository relationships that help
localize a failure. Avoid marketing language and exhaustive API inventories.

### Diagnostic lifecycle

Describe the relevant provisioning, initialization, reconciliation, test, and
cleanup sequence. Treat it as a diagnostic sequence, not a guaranteed order.
Require resource conditions and timestamped logs to prove where progress
stopped. When a dependency chain matters, state that a downstream symptom does
not establish the upstream cause.

### Test and job flavors

Document meaningful test families and environment flavors. Require the analyzer
to identify the actual flavor from the job and artifacts before applying
flavor-specific guidance. Put unknown flavors under `Unresolved details`.

### Artifact layout

List exact project-specific paths or path patterns only when project evidence
supports them. State what each artifact can prove. The base prompt already
covers universal Prow files such as `build-log.txt`; label them as engine-owned
defaults when they appear here. Require the analyzer to list the available
artifact tree before declaring that an expected file is absent.

### Common failure patterns

Write operational rules rather than a catalog of possible failures. Each rule
should identify:

1. The symptom or signal.
2. The evidence that must be read.
3. The incorrect causal conclusion to avoid.
4. The remediation boundary supported by that evidence.

A useful form is: "If X appears, read Y and Z before concluding A. Do not infer
A from X alone."

### Transient classification

Add a transient rule only when project evidence supports it. Every rule must
state both the positive run evidence that permits `is_transient=true` and the
evidence or persistence that makes the failure non-transient. A failure is not
transient merely because a retry might recover.

Do not broadly classify invalid credentials, persistent quota exhaustion,
unavailable SKUs, deterministic bootstrap failures, repeated missing images,
lasting webhook TLS failures, or service startup failures that never recover
during the run as transient.

### Triage order

Provide an ordered, artifact-first procedure. Start with the failing JUnit
detail and `build-log.txt`, narrow to resource conditions and relevant component
logs, then compare with a passing resource or build when possible.

### Relevant source repositories

List only repositories established by project evidence that can produce
actionable `relevant_files` paths. Prefer GitHub `owner/name` form and do not
invent repository names.

### Unresolved details

List important artifact paths, flavors, dependency chains, failure boundaries,
or repositories that the available sources do not establish. Use factual
maintainer TODOs instead of filling gaps with generic assumptions.

## Analyzer capability boundary

The analyzer can read supplied Prow artifacts through engine tools. Optional
Kubernetes tools navigate Kubernetes-shaped logs and resource dumps already
captured in the artifact tree. They do not connect to a live Kubernetes API.
The analyzer also does not have portal, SSH, arbitrary shell, browser, or local
CLI access. Do not present an unavailable investigation as evidence already
collected, and do not substitute retries or manual portal checks for an
artifact-backed remediation.

## Worked examples

Two production consumer prompts are available as content references. Use them
to judge diagnostic depth, then map grounded facts into the required section
structure above.

- **Provider-agnostic example (CAPD, Docker):**
  [CAPI core `prompts/system.md`][capi-prompt] is ~150 lines covering
  provider-agnostic CAPI architecture, the CAPD Docker provider, three job-type
  families (E2E / unit / conformance), and the per-spec workload-cluster artifact
  layout. Use this as the starting template for most projects.
- **Cloud-VM example:** [a VM-based provider `prompts/system.md`][capz-prompt] is
  ~100 lines covering cloud VM provisioning, kubeadm control-plane init, 14
  documented failure patterns, provider-specific transient errors, and that
  provider's artifact layout. Use it when cloud-VM failure patterns dominate.

[capi-prompt]: https://github.com/willie-yao/capi-prow-ai-dashboard/blob/main/prompts/system.md
[capz-prompt]: https://github.com/willie-yao/capz-prow-ai-dashboard/blob/main/prompts/system.md

A minimal docs-only example also lives in [`configs/example/`][example] in
this engine repo.

[example]: ../configs/example

## Tips

- **Keep it factual.** The model treats your prompt as ground truth. Do not
  speculate about failure modes you have never seen.
- **Quote real log lines.** Where you list a failure pattern, include the
  exact log message the model should match on. Vague descriptions ("CNI
  doesn't start") produce vague summaries.
- **Use markdown headings.** The model uses your section structure to
  organize its reasoning.
- **Length is fine.** 100-300 lines is normal. Beyond that you may start
  crowding out the per-failure evidence in the context window; trim aggressively
  if you see the model ignoring the user message.
- **Iterate against real failures.** Trigger an AI analysis on a known
  failure, read the summary, and refine the prompt where the model got it
  wrong. The updated prompt applies to new analyses. Existing reusable entries
  keep the prompt fingerprint that produced them.

## Iterating on the prompt

Editing `prompts/system.md` changes the prompt used for new analyses. Existing
reusable entries remain cached and keep their original `prompt_hash` provenance.
This avoids an automatic dashboard-wide re-analysis after a prompt edit.

Set `ai.cache_generation` or `AI_CACHE_GENERATION` to a new value when a prompt
rewrite requires an intentional full rebaseline. Returning to a prior value
reuses its unexpired entries. Use the **Clear AI Cache** workflow only for
emergency destructive cleanup.

## What the engine does NOT add to your prompt

It is worth being explicit: the engine intentionally does not own any
project-specific opinion. That includes:

- The list of components that exist in your project
- Architecture diagrams or dependency chains
- Failure patterns specific to your CI fleet
- Cloud-provider-specific transient errors (quota throttling, vCenter
  timeouts, etc.)
- Test-flavor-specific debugging instructions

If you want the model to know any of that, it must be in your
`prompts/system.md`.

One adjacent knob you may also want, **outside** the system prompt: the
agentic loop configuration in `project.yaml` under `ai`. The loop's tool
budget, evidence floors, and critique/skills gates all live there. See
[docs/agentic.md](agentic.md) for the full reference and
[docs/skills.md](skills.md) for recipe-driven evidence requirements.
