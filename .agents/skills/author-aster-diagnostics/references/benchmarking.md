# Held-out benchmark and reporting contract

## Contents

- [Protect the blind boundary](#protect-the-blind-boundary)
- [Record prompt-authoring validation](#record-prompt-authoring-validation)
- [Freeze authoring output](#freeze-authoring-output)
- [Prepare condition copies](#prepare-condition-copies)
- [Validate derived manifests without a provider](#validate-derived-manifests-without-a-provider)
- [Run the benchmark matrix](#run-the-benchmark-matrix)
- [Evaluate without tuning](#evaluate-without-tuning)
- [Write deterministic results](#write-deterministic-results)

## Protect the blind boundary

Before authoring, create a denylist and append-only access log. Use
`scripts/blind_access.py` to mediate local filesystem reads when the package will
claim wrapper enforcement. It does not mediate remote GCS or HTTP reads. Treat
those as `self_reported` unless the evidence was first copied into the controlled
local tree. Record `wrapper_enforced` or `self_reported`; the
latter must state that it cannot prove all reads. The denylist must cover locked
benchmark manifests, benchmark tests that name cases or signals, prior diagnoses,
scoring and forbidden files, manual recipes, and previous evaluation outputs. Any pre-freeze request for those categories must be blocked
and logged. Use
[benchmark-manifest.schema-only.json](benchmark-manifest.schema-only.json) for
identity-manifest shape. Do not read an answer-bearing test to discover fields.
Give prompt authors only the authoring split and fresh validation sessions only
the proposed prompt plus raw validation evidence. Keep final-holdout cases
excluded until freeze.

After prompt and proposal hashes are frozen, use a separate evaluator for
answer-bearing material. Preserve the frozen pre-freeze access log and record
post-reveal reads separately in evaluator output. If a scoring overlay will
produce a locked score, a separate scoring author must freeze it before the prompt
scorer reads condition prompts or outputs. Do not reuse the authoring agent
as the blind evaluator. Anonymize project and model labels when practical.

## Record prompt-authoring validation

Keep same-author deterministic validation, independent fresh holdouts, and the
dashboard provider benchmark as separate evidence planes.
For each validation case, record the prompt hash, anonymous evaluator label,
input split, initiating-error result, causal-chain result, transient result,
citations, ownership discipline, duration, usage when reported, and whether the
session saw any expected answer.

Fresh-session validation may justify another prompt revision. It cannot establish
engine condition B or C behavior, cache acceptance, Tool compatibility, or
cross-project regression safety. Preserve those as separate benchmark outcomes.

## Freeze authoring output

Before any final-holdout result is revealed, record SHA-256 hashes for:

- Existing and proposed `prompts/system.md`.
- A `prompt_regression` inventory showing retained, updated, removed, and deferred
  baseline rules with supporting cases.
- `baseline_provenance` entries for every known job, build, test, causal event,
  and hashed report or source that informed the baseline prompt.
- Existing active recipe files and the engine-computed merged skill-set hash.
- Every proposal file.
- The authoring report and applicability matrix snapshot.
- Engine commit, consumer commit when the consumer is a Git repository, source
  commit, test-infra revision, selected jobs, authoring builds, validation builds,
  final-holdout builds, and artifact manifests. Record a non-Git consumer with
  null commit and `commit_status: not_applicable`.
- A deterministic validation-engine file manifest whose entries contain relative
  path, file mode, Git blob ID, and SHA-256.
- An identity-only A/B/C manifest for every final holdout.
- For an uncommitted skill, the skill manifest and companion anchor-test or schema
  files required to validate that exact snapshot.

Use a timestamped evidence directory and append-only result files. Do not delete
failed or malformed trials. Generate the baseline and final validation-engine file
manifests with `scripts/write_validation_file_manifest.py`; require exact document
equality rather than relying on later plain `git diff` output.

## Prepare condition copies

Never modify the original public evaluation consumer. Create separate disposable
Git copies for each condition:

- **A:** existing prompt plus existing active skills.
- **B:** proposed prompt plus existing active skills.
- **C:** proposed prompt plus proposed recipes copied into active `skills/` only
  inside the disposable copy.

Keep `proposals/skills/` unchanged in every authoring output. Condition C is a
benchmark-only activation, not promotion.

Create one condition-specific identity manifest per A/B/C copy for every final
holdout. Build it from the bundled schema-only fixture and the frozen corpus
identity. It may contain case, job, build, condition, consumer, project, prompt,
and active-skill hashes. It must not contain a reference diagnosis, expected
answer, scoring rules, forbidden rules, signal labels, or manual recipe. Commit
each condition copy so clean-tree and identity checks remain effective. Hash
every identity manifest.

After causal reveal, create a separate scoring overlay that binds the reference
diagnosis, scoring rules, and forbidden rules for that case. Never rewrite the
identity manifest to add answer-bearing fields. Before reveal, record the overlay
status as `not_revealed` with null path and hash.

## Validate derived manifests without a provider

Validate the schema-only fixture and every derived identity manifest with
`report-schema.json` and `scripts/validate_reports.py`. This path validates shape
without opening an answer-bearing benchmark test. Also load each disposable
consumer project and merged recipes through an existing non-answer-bearing engine
API when one is available, recording the full skill-set hash and IDs.

If the pinned engine has no documented provider-free identity loader, record a
generic engine gap. Do not discover the contract by reading tests that name locked
cases or signals. Do not run a provider benchmark merely to validate identity.

## Run the benchmark matrix

Use the selected engine revision's documented benchmark entry point or a command
provided by the independent post-reveal evaluator. Do not inspect answer-bearing
benchmark tests during authoring merely to reconstruct environment variables. A
typical single-case cold run from `<engine>/backend` is:

```bash
RUN_AI_BENCHMARK=1 \
AI_API=<api-mode> \
AI_ENDPOINT=<endpoint> \
AI_MODEL=<model> \
AI_TOKEN=<available-in-environment> \
BENCH_MANIFEST=<condition-manifest.json> \
BENCH_CASE=<case-id> \
BENCH_PROJECT_DIR=<condition-consumer> \
BENCH_REPETITIONS=3 \
BENCH_CACHE_MODE=cold \
BENCH_CACHE_DIR=<private-condition-cache> \
BENCH_RESULTS_JSONL=<private-condition-results.jsonl> \
BENCH_MODEL_LABEL=<anonymous-stable-label> \
go test ./internal/e2e -run '^TestAIBenchmark$' -v -count=1 -timeout 60m
```

Never place a token value directly in a report or command transcript. Use a
private environment or secret provider. Do not inspect Secret values.

Use separate cache directories and JSONL files for every condition. Prompt,
recipe, model, and transient-streak changes do not reliably refresh reusable
analyses by themselves, so never share a cache across conditions. Keep condition
labels blind during evaluation. Run at least three repetitions when provider
access and cost permit. Record cold-cache state, cache generation, duration,
usage, provider attempts, tool failures, malformed calls, citations, source
grounding, initiating error, transient outcome, and score dimensions.

Before creating conditions, reject every final holdout whose job and build or
causal-event identity overlaps `baseline_provenance`. A different test in the
same build is still excluded because the baseline author may have seen sibling
evidence. Run A, B, and C for every remaining final holdout, including recurrence
and out-of-class cases, plus an unrelated control. Prefer one analyzer or test event
per holdout identity. If a build-level holdout reveals independent failures,
score and classify each event separately and aggregate recurrence plus
generalization as `mixed`. Do not reuse previous holdout IDs in a new package.

## Evaluate without tuning

Compare A, B, and C separately so prompt value is not attributed to a recipe.
Score initiating-error accuracy, ownership, tri-state transient classification,
citation validity, source grounding, storage identity correlation when applicable,
and causal overclaiming. For build-level holdouts, score every revealed causal
event rather than one merged job summary. Reclassify each holdout after reveal by comparing its
causal class with the frozen authoring and validation classes. Do not use job
family, duration, or wrapper similarity as the definition.

Do not revise a proposal after seeing its final-holdout answer and then count the
same case as blind validation. Preserve failures. Do not call a regex score
`locked` when the same evaluator wrote the overlay after reading the prompt. Use
`scoring_protocol: same_evaluator_post_hoc`, set `locked_score` to null, and
retain only the manual semantic score in that situation. A later revision requires a new
final holdout or an `experimental` or `unresolved` classification.

Do not count a usable-looking text response as a successful trial when tool calls
were malformed or unexecuted, required artifact reads were zero, finalization was
synthesized, critique had hard failures, quality floors failed, or cache
persistence was rejected. Record the structured provider limitation and keep the
behavioral conclusion unresolved.

Read any embargoed manual comparison only after frozen proposal hashes exist.
Use it as a comparison, not as authoring evidence and not as proof that the
independent proposal works.

## Write deterministic results

Write `reports/benchmark-results.json` with the exact version-2 contract in
[report-schema.json](report-schema.json). Validate it together with the failure
corpus using:

```bash
python3 <skill>/scripts/validate_reports.py \
  <authoring-root>/reports/failure-corpus.json \
  <authoring-root>/reports/benchmark-results.json \
  --evidence-root <private-evidence-dir>
```

The report keeps these evidence planes separate:

- `authoring_validation`: prompt checks performed during bounded authoring, with
  `review_mode` distinguishing `same_author_review` from `fresh_session`.
- `fresh_holdout_trials`: independent fresh-agent diagnoses after freeze, with a
  schema-validated diagnosis JSON, event-level causal kinds, an aggregate
  post-reveal kind, and a reclassification flag.
- `dashboard_trials`: real engine A/B/C and control operations.
- `condition_manifests`: one identity-only A/B/C manifest and command per final
  holdout plus controls, with a separate post-reveal scoring-overlay binding.

Do not place fresh-agent trials in `dashboard_trials` or use them to claim
dashboard behavior. Completed fresh holdouts and dashboard trials always record a manual semantic
score. They record a locked lexical score only when `scoring_protocol` proves that
a distinct scoring author froze the overlay before the prompt scorer had access. Each classification records exactly one evidence plane and supporting
IDs. `recommended` requires repeated cold dashboard trials and a passed
dashboard-provider control.

Authoring-time classifications remain frozen in `failure-corpus.json`. Final
holdout or provider classifications live in `benchmark-results.json`; do not
rewrite the corpus after freeze to make them match.

The `clean-validation-engine` check compares deterministic baseline and final
file manifests generated after any intentional skill snapshot sync. Both contain
relative paths, file modes, Git blob IDs, and SHA-256 values. Require exact
manifest equality. A status snapshot or plain binary diff is supporting output,
not the reproducibility contract.

Every proposal records at least two `prompt_only_misses`, each with distinct
`causal_event_id` and `fresh_session_id`. Existing consumer recipes are not trusted
quality exemplars and cannot waive this threshold. Every validation command with
a passed, failed, or partial status records a preserved output path and SHA-256.
The shared `freeze_manifest` object binds the reports to the frozen evidence set.

Keep provider JSONL and other private trial artifacts outside a public consumer.
Trial paths are relative to the explicitly supplied private `--evidence-root`;
validation logs, freeze manifests, and proposal files remain relative to the
consumer root.

Use exact enum values and arrays rather than embedding classifications in prose.
Represent unavailable evidence with explicit statuses and limitations. Keep
private provider or artifact content out of reports and validation logs.
