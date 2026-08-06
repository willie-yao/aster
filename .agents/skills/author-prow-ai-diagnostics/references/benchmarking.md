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

During authoring, do not read or expose locked reference diagnoses, expected
answers, manual recipes, scoring regexes, forbidden-answer regexes, or prior
final-holdout dashboard output. Give prompt authors only the authoring split.
Give fresh validation sessions the proposed prompt plus raw validation evidence,
not the corpus diagnosis. Keep final-holdout cases excluded until freeze.

After prompt and proposal hashes are frozen, use a separate evaluator for
answer-bearing material. Do not reuse the authoring agent as the blind evaluator.
Anonymize project and model labels when practical.

## Record prompt-authoring validation

Keep fresh LLM CLI validation distinct from the dashboard provider benchmark.
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
- Existing active recipe files and the engine-computed merged skill-set hash.
- Every proposal file.
- The authoring report and applicability matrix snapshot.
- Engine commit, consumer commit, source commit, test-infra revision, selected
  jobs, authoring builds, validation builds, final-holdout builds, and artifact
  manifests.

Use a timestamped evidence directory and append-only result files. Do not delete
failed or malformed trials.

## Prepare condition copies

Never modify the original public evaluation consumer. Create separate disposable
Git copies for each condition:

- **A:** existing prompt plus existing active skills.
- **B:** proposed prompt plus existing active skills.
- **C:** proposed prompt plus proposed recipes copied into active `skills/` only
  inside the disposable copy.

Keep `proposals/skills/` unchanged in every authoring output. Condition C is a
benchmark-only activation, not promotion.

The current frozen benchmark manifest pins consumer commit and file hashes. Do
not edit it in place. After freezing, create one condition-specific manifest per
copy by retaining the selected case, fixture, source, scoring, and reference
fields and updating only the disposable consumer commit, `project.yaml` hash,
and prompt hash. Commit each condition copy so the benchmark's clean-tree and
identity checks remain effective. Hash every derived manifest.

## Validate derived manifests without a provider

The committed manifest tests do not open condition-specific manifests. Before
any provider run, create a temporary Go test only in the disposable validation
engine clone under `backend/internal/e2e`. For every condition, call the existing
`loadBenchmarkManifest` on its derived manifest and
`validateBenchmarkProjectDir` on its clean consumer directory. Also load the
consumer project and merged recipes to record the full skill-set hash and IDs.

Run the temporary test without `RUN_AI_BENCHMARK`, record the result, then remove
it from the disposable clone. Do not set `BENCH_MANIFEST` on `TestAIBenchmark`
merely to validate identity because that test requires provider coordinates
before loading the manifest.

## Run the benchmark matrix

Read the selected engine revision's `backend/internal/e2e/benchmark_test.go` and
manifest tests for current environment variables. A typical single-case cold run
from `<engine>/backend` is:

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

At minimum include the target final-holdout case and an unrelated control. For the
frozen cross-project suite, include Kueue, GCP PD CSI, and Secrets Store CSI.

## Evaluate without tuning

Compare A, B, and C separately so prompt value is not attributed to a recipe.
Score initiating-error accuracy, ownership, transient classification, citation
validity, source grounding, and causal overclaiming.

Do not revise a proposal after seeing its final-holdout answer and then count the same
case as blind validation. Preserve failures. A later revision requires a new
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

Write `reports/benchmark-results.json` as one JSON object with separate
`authoring_validation` and engine `trials` arrays. At minimum include:

```json
{
  "schema_version": 1,
  "engine_commit": "<sha>",
  "recipe_schema_version": "not_applicable",
  "consumer": {
    "repository": "owner/name",
    "commit": "<sha>",
    "project_sha256": "<sha256>",
    "existing_prompt_sha256": "<sha256>",
    "proposed_prompt_sha256": "<sha256>",
    "existing_skill_set_hash": "<engine hash>"
  },
  "corpus": {
    "sha256": "<sha256>",
    "authoring_cases": [],
    "validation_cases": [],
    "final_holdout_cases": [],
    "coverage_limitations": []
  },
  "prompt_versions": [],
  "authoring_validation": [],
  "proposals": [
    {
      "id": "candidate-id",
      "path": "proposals/skills/candidate-id.yaml",
      "sha256": "<sha256>",
      "classification": "unresolved",
      "classification_reasons": ["provider_unavailable"],
      "applicability": {
        "passed": 10,
        "failed": 0,
        "cases": []
      }
    }
  ],
  "conditions": [],
  "trials": [],
  "controls": [],
  "provider": {
    "available": false,
    "api_mode": "unresolved",
    "model_label": "unresolved",
    "input_tokens": 0,
    "output_tokens": 0,
    "duration_ms": 0,
    "critique_cache_policy": "unresolved",
    "limitations": []
  },
  "generic_engine_gaps": []
}
```

Use exact enum values and arrays rather than embedding classifications in prose.
Represent a missing run with an explicit status and reason. Keep private provider
or artifact content out of this report.
