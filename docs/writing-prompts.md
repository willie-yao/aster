# Writing a project AI prompt

Every consumer of [prow-ai-dashboard][engine] must ship a `prompts/system.md`
alongside its `project.yaml`. This file is what makes the AI summaries useful
for your project: it tells the model how your components fit together, what
common failures look like, and which signals to skip as known flakes.

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
  with no editorial filtering — what you write is what the model sees.
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

## Recommended sections

The sections below are what consistently lift summary quality. Pick the ones
that apply; you do not have to use all of them.

### Architecture
5-15 bullets describing how the system under test fits together: top-level
controllers, the resource hierarchy, addons, and the canonical request flow on
a successful run. The model uses this to interpret stack traces and resource
YAMLs.

### Project-specific artifact layout
If your project drops debug data under `artifacts/` in a project-specific
shape (e.g. `artifacts/clusters/{cluster}/machines/{vm}/kubelet.log`), list
the paths the model should consult. The base prompt already covers the
universal Prow files; do not repeat them.

### Common failure patterns
5-15 failure modes the model is likely to encounter, grouped by component.
For each, name the signal that distinguishes it. Concrete log-line excerpts
are more useful than abstract descriptions.

### Transient errors
A bullet list of patterns that should set `is_transient=true` rather than
being flagged as bugs (API throttling, quota exhaustion, image-pull backoff,
DNS flakes, etc.). The engine's regex-based `IsKnownTransient` covers the
most universal cases; add anything your project sees often.

### Triage order
If the universal triage order in the base prompt isn't enough (e.g. for a
provider where a specific log file usually has the answer), spell out the
order. Example: "1. build-log.txt → 2. kubelet.log → 3. cloud-init-output.log
→ 4. <provider> activity log → 5. Machine resource YAML."

### Repos to reference in `relevant_files`
The engine's response schema asks the model for a list of file paths to
investigate. Tell it which GitHub repos are in scope so the paths it returns
are actionable links rather than guesses.

## Worked examples

Two production consumer prompts are available as reference templates.
Both are around 100-150 lines and follow the section structure above.

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
  wrong. Prompt fingerprinting refreshes affected analyses on the next run.
  Clear the cache only when you want an immediate full rebaseline.

## Iterating on the prompt

Editing `prompts/system.md` takes effect automatically: each analysis records a
fingerprint of the prompt it was produced under, and on the next run any failure
whose prompt no longer matches is re-analyzed. You do **not** need to clear the
cache, and you do not lose existing analyses while re-analysis catches up: an
old analysis stays published until its replacement succeeds (if a re-analysis
errors or times out, the prior result is kept).

Re-analysis is incremental, so a large dashboard refreshes over one or more
deploys rather than all at once. To force every analysis to refresh immediately
(e.g. to re-baseline after a big prompt rewrite), run the **Clear AI Cache**
workflow from the Actions tab; that wipes the cache so the next deploy
re-analyzes everything from scratch.

```yaml
# In your consumer repo, e.g. .github/workflows/clear-cache.yml
on: { workflow_dispatch: {} }
jobs:
  clear:
    uses: willie-yao/prow-ai-dashboard/.github/workflows/reusable-clear-cache.yml@main
    with:
      project_dir: .
```

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
