# Authoring AI skills (recipes) for your project

> Status: Consumer-side opt-in. Skills extend the always-on critique gate, so
> you only need this doc if you want to harden the gate against the specific
> failure patterns your CI hits.

This doc explains how to author and ship diagnostic recipes (called
"skills" in the engine) that bias the AI loop toward reading the
evidence your project considers canonical for known failure modes.

## What a skill is

A skill is a YAML file at `<your-project-dir>/skills/<name>.yaml`
that declares:

1. **Triggers:** regex patterns that, when any matches the model's
   draft analysis (root_cause + summary + suggested_fix + relevant_files),
   marks the recipe as "applicable" to this failure.
2. **Required evidence:** one or more groups of regex patterns. For
   each group, the agent must have successfully read at least one
   artifact whose path matches one of the group's patterns. A group
   is satisfied by any single match.
3. **Procedure:** markdown guidance quoted back to the model when
   the recipe matches but evidence is still missing. Treated as
   *consumer guidance*; the engine wraps it with a disclaimer when
   injecting it so the recipe cannot accidentally override the
   system prompt or response schema.

The critique gate consults the loaded skill set on every draft. When
a recipe matches and any of its evidence groups is unsatisfied, the
gate appends a feedback message naming the recipe, listing the
missing groups, and quoting the procedure. The agentic loop then
re-prompts the model and dynamically extends its retry budget so the
model has room to actually go read the missing artifacts.

## When to author a skill

A skill is the right tool when **all** of the following are true:

- The same failure pattern reappears across multiple builds.
- A canonical diagnostic procedure exists (e.g. "for an x509 webhook
  failure, always look at the cert-manager Certificate config and
  the webhook server secret").
- The weaker AI model used by the consumer (e.g. Qwen3-235B) stops
  short of that procedure even with the engine's prompt-side rules.

If the model already does the right thing on this failure pattern,
do not author a skill: extra triggers just inflate the recipe set
hash and invalidate cache for no benefit.

## Schema

```yaml
# REQUIRED. Unique identifier within your project's skill set.
# Kebab-case, e.g. webhook-tls-failure, machine-bootstrap-empty-logs.
id: webhook-tls-failure

# Optional human-readable label. Defaults to id. Surfaced in feedback.
name: Webhook TLS failure

# Optional one-line guidance to the recipe author. Not shown to the
# model; documentation only.
description: |
  CAPI bootstrap webhook fails with x509 errors during workload-cluster
  create.

# Optional priority for ordering when multiple recipes match. Higher
# first; default 100. Use to pin a specific recipe ahead of a broader
# one.
priority: 200

# REQUIRED. Regex patterns OR'd together. Matched against the joined
# (root_cause + summary + suggested_fix + relevant_files) text of the
# model's draft. Use (?i) for case-insensitive matching.
triggers:
  - "(?i)x509:?\\s*certificate"
  - "(?i)webhook.*tls"

# Optional but usually present. Evidence groups the agent must satisfy
# before critique accepts a draft that matched this recipe. Each group
# is satisfied if any single regex matches any path the agent
# successfully read.
required_evidence:
  - id: cert-manager-config
    description: cert-manager Certificate or Issuer config
    any_of:
      - "config/certmanager/.*\\.ya?ml"
      - ".*certificate\\.ya?ml"
  - id: webhook-secret
    description: webhook server cert secret contents
    any_of:
      - ".*webhook.*secret.*"

# Optional markdown guidance quoted back to the model on retry. Keep
# short and tool-oriented: name the canonical artifacts and the
# specific signals to look for. Do NOT issue blanket instructions
# that contradict the engine system prompt (the engine wraps this
# block with a "consumer guidance, not engine instruction" disclaimer
# but a well-scoped procedure is still better).
procedure: |
  1. List cert-manager Certificate objects:
     kubectl get certificate -A
  2. Inspect the webhook server cert secret in the bootstrap cluster
     under artifacts/clusters/bootstrap/logs/cert-manager-system/.
  3. Compare the Certificate DNS names to the webhook service DNS
     name from the webhook configuration manifest.
```

## Loading semantics

Skills are loaded once at fetcher startup from
`<project-dir>/skills/*.yaml`:

- Missing directory → empty set, no error. Skills are opt-in.
- Empty directory → empty set, no error.
- Any present `.yaml` file must parse cleanly with strict YAML
  (unknown fields are errors). Any failure aborts fetcher startup.
- Every regex must compile. Compile failures abort startup.
- Duplicate IDs across files abort startup.

The engine logs a one-line summary on load:
```
Loaded 7 AI skill recipe(s) from ./skills/ (hash=a1b2c3d4)
```

## Enabling

Skills are loaded regardless of any flag (so a parse error catches a
broken recipe before runtime). There is no `skills.enabled` flag:
shipping recipe files under `<project_dir>/skills/*.yaml` is the opt-in.
Skills extend the always-on critique gate when recipes are present:

```yaml
# project.yaml
ai:
  critique:
    max_retries: 2  # optional; default 2
```

Skills extend critique; they don't replace it. If you ship recipes, the
gate uses them to require the evidence paths they describe.

## Cache invalidation

Each cache entry is stamped with the SHA-256 fingerprint of the
loaded skill set at write time (`SkillSetHash` field). On the next
fetcher run:

- If no skills are loaded, the hash check is skipped (cache unaffected
  by recipe set changes).
- If skills are loaded, cache entries whose stored hash differs from the
  currently-loaded set's hash are invalidated and re-analyzed.

Editing a recipe, even a single character in a trigger regex or procedure,
invalidates every cache entry on the next run.
The fingerprint is whitespace- and comment-insensitive, so reformatting
a YAML file does not bust the cache.

## Writing good triggers

Triggers fire against the model's *draft* analysis, not against
artifact contents. Tune them to phrases the model actually emits
when it's diagnosing this failure pattern:

| Pattern               | Good for                                  | Risk                              |
|-----------------------|-------------------------------------------|-----------------------------------|
| `(?i)x509`            | webhook TLS / cert errors                 | over-fires on benign mentions     |
| `(?i)cloud-init.*empty` | empty bootstrap logs                    | narrow; misses paraphrase         |
| `(?i)leader\s+election` | KCP control-plane leader-election loss | narrow; very specific             |
| `\bquota\b`           | quota exhaustion                          | watch for "quota" used elsewhere  |

Tradeoffs:

- **Wider triggers** catch more cases but waste critique cycles on
  failures the recipe doesn't actually help with.
- **Narrower triggers** are tight but miss paraphrases the model
  might use.

Start narrow. Widen when you observe a real miss in the data; never
widen on speculation.

## Writing good evidence groups

Each group should encode "if this failure pattern is real, the
canonical artifact for it lives at one of these paths":

- Patterns are matched against the agent's successfully-read paths
  (full path, lowercase, slash-normalized). Use slash-style globs
  (e.g. `clusters/.*/machines/`).
- Use `any_of` to handle natural variation: different namespaces,
  different generated filenames, different controller pod names.
- Prefer 2-3 evidence groups over a single sprawling group: smaller
  groups give more precise feedback to the model.
- Keep `description` short and human-readable; the engine surfaces
  it verbatim in critique feedback.

## Dynamic retry budget

When a recipe matches and the agent has missing evidence, the
critique gate appends:

- The standard `critiqueRetryIters` budget (3 extra iterations).
- A skill-driven bonus: `1 + 2*N` extra iterations, where N is the
  total number of missing evidence groups, capped at
  `critiqueMissingEvidenceBonusCap` (6 by default).

So a recipe with 1 missing group gets `3 + 3 = 6` extra iters per
retry; a recipe with 3 missing groups gets `3 + 6 = 9` extra iters.
The cap prevents pathological recipes (10+ groups) from giving the
loop unbounded budget.

## Schema versioning

Skills don't have their own schema version. Changes to a recipe
change the SkillSetHash, which invalidates affected cache entries.
Engine-side contract changes (e.g. adding a new check inside the
critique gate) bump `currentCritiqueVersion` instead, which also
invalidates all entries on the next run.

## Authoring checklist

Before merging a new recipe:

1. **Trigger fires on a real draft.** Run `grep -i <trigger>` against
   `data/jobs/*.json` to confirm at least one analysis uses the phrase.
2. **Evidence groups match real reads.** Check the `tool_calls` of a
   matching analysis (or run a local fetch) to confirm the agent does
   fetch the artifact when prompted.
3. **Procedure is short and tool-oriented.** Quote canonical tool
   names + paths. Don't issue meta-instructions ("think carefully").
4. **`min_gcs_bytes` is high enough** that the cumulative
   tool-call budget already covers the canonical reads. Otherwise
   the agent will satisfy `min_gcs_bytes` with shallow listings and
   never reach the depth your recipe expects.
5. **Validated before promotion.** Refetch with the recipe present
   and confirm the recipe-matched cases gain evidence reads and
   substantive root-cause depth versus the prior run.

## Observability

When a recipe fires, the fetcher logs (per analysis):

```
  ✗ agentic critique: [skill:webhook-tls-failure(missing:cert-manager-config,webhook-secret)]; re-prompting (retry 1/2, +9 iters)
```

After the run, every `AIAnalysis` in `data/jobs/*.json` carries:

- `critique_passed`: did the final answer clear critique?
- `critique_version`: which engine contract version did it clear?
- `skill_set_hash`: fingerprint of the recipe set at the time.

Grouping analyses by `skill_set_hash` lets you compare runs from
before and after a recipe change without re-fetching unchanged entries.
