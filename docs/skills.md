# AI diagnostic skills and project recipes

> Status: Engine profiles plus consumer extensions. Skills extend the always-on
> critique gate and deterministic evidence planner.

Skills are YAML diagnostic recipes that steer the analysis toward canonical
evidence. A recipe contains trigger regexes, required artifact-path groups, and
a short investigation procedure.

The engine composes one merged set for the dashboard-owned analyzer:

1. **Engine Prow profile.** Always enabled because the product analyzes Prow
   runs. It teaches the distinction between `build-log.txt` and JUnit details,
   the roles of Prow metadata files, effective job configuration and test
   selection, artifact-tree navigation, timeline ordering, cleanup noise, and
   last-passing comparisons.
2. **Engine Kubernetes profile.** Enabled when the effective `ai.tools`
   selection includes `k8s` or an individual `k8s.*` tool. It contains
   provider-neutral procedures for Machine and Node initialization, Pod
   startup, cluster provisioning, and Service, API, DNS, and kube-proxy
   connectivity.
3. **Consumer recipes.** Loaded from
   `<project-dir>/skills/*.{yaml,yml}` for project and provider knowledge.

These are composition sources, not precedence layers. The complete merged set
is sorted by priority descending and ID ascending, and only recipes whose
triggers match a model draft are surfaced for that draft.

The universal system prompt stays small. The Prow profile is engine-owned
because Prow artifacts are part of the product contract. Kubernetes knowledge
is conditional because some Prow consumers do not produce Kubernetes cluster
dumps or use the cluster-navigation tools. Provider details such as CAPZ,
Azure resources, ASO, and project flavor behavior remain consumer-owned. The
failure prompt may name the current test-infra config file and discovery
revision, while `prowjob.json` remains the evidence for what the failed run
actually executed.

## What a skill is

Each recipe declares:

1. **Triggers:** regex patterns matched against the model draft
   (`root_cause`, `summary`, `suggested_fix`, and `relevant_files`).
2. **Required evidence:** groups of artifact-path regexes. Each group is
   satisfied when the agent successfully reads one matching path. Initial-plan
   coverage requires the content returned by that read to be non-empty.
3. **Procedure:** short diagnostic guidance returned when a recipe matches.

Procedures are untrusted guidance. They cannot override the system prompt, Tool
constraints, the result schema, or the tool budget. The critique gate wraps
procedures with this boundary.

The analyzer matches the bounded failure signal before iteration one, resolves
ranked candidates with `skills.Set.Plan`, and prepends the bounded plan. The gate
uses those candidates for deterministic critique repair, with a bounded tree-walk
fallback for unresolved or newly matched groups.

## When to author a skill

A skill is the right tool when **all** of the following are true:

- The same failure pattern reappears across multiple builds.
- A canonical diagnostic procedure exists (e.g. "for an x509 webhook
  failure, always look at the cert-manager Certificate config and
  the webhook server secret").
- The weaker AI model used by the consumer (e.g. Qwen3-235B) stops
  short of that procedure even with the engine's prompt-side rules.

If the model already does the right thing on this failure pattern,
do not author a skill: extra triggers add maintenance cost and change the
provenance hash for no benefit.

## Schema

```yaml
# REQUIRED. Unique consumer identifier within the merged skill set.
# Do not use the reserved engine. prefix. Kebab-case is recommended,
# e.g. webhook-tls-failure or machine-bootstrap-empty-logs.
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
# successfully read. An optional when list limits a group to matching
# drafts, so one recipe can require different evidence by failure class.
required_evidence:
  - id: cert-manager-config
    description: cert-manager Certificate or Issuer config
    when:
      - "(?i)certificate|issuer|x509"
    any_of:
      - "config/certmanager/.*\\.ya?ml"
      - ".*certificate\\.ya?ml"
    # Optional positive content proof. Path and content predicates must match
    # the same artifact. Use (?i) for case-insensitive matching.
    content_any_of:
      - "(?i)kind:\\s*(Certificate|Issuer)"
    content_all_of:
      - "(?i)dnsNames"
  - id: webhook-secret
    description: webhook server cert secret contents
    any_of:
      - ".*webhook.*secret.*"

# Optional markdown guidance quoted back to the model on retry. Keep
# short and tool-oriented: name the canonical artifacts and the
# specific signals to look for. Do NOT issue blanket instructions
# that contradict the engine system prompt. The engine wraps this block
# with a guidance-only disclaimer, but a well-scoped procedure is still
# better.
procedure: |
  1. List cert-manager Certificate objects:
     kubectl get certificate -A
  2. Inspect the webhook server cert secret in the bootstrap cluster
     under artifacts/clusters/bootstrap/logs/cert-manager-system/.
  3. Compare the Certificate DNS names to the webhook service DNS
     name from the webhook configuration manifest.
```

## Loading and composition

Skills are loaded once at startup. The fetcher and Orka producer call the same
merged loader with the same effective tool selection.

Loading is strict:

- Every selected engine recipe is embedded in the binary and parsed with the
  same schema as consumer recipes. A malformed built-in is a startup error.
- A missing or empty consumer `skills/` directory is allowed unless
  `ai.consumer_skills` requires the bundle or a minimum recipe count.
- Every present `.yaml` or `.yml` file must parse with strict YAML. Unknown
  fields, invalid regexes, and read errors abort startup.
- IDs must be unique across the complete merged set. The `engine.` prefix is
  reserved for built-ins, so consumers cannot silently replace engine recipes.
- The final order is deterministic: priority descending, then ID ascending.
  Source filename and profile load order do not affect the result.

The engine logs the selected profiles and merged hash:

```text
Loaded AI skills (profiles=prow,kubernetes engine=6 consumer=11 consumer_bundle=true hash=a1b2c3d4)
```

## Profile selection and Kubernetes opt-out

The Prow profile is always active. The Kubernetes profile follows `ai.tools`:

```yaml
ai:
  tools: [filesystem, k8s]  # Prow plus Kubernetes profiles
```

For a consumer where Kubernetes recipes are inappropriate, keep only the
filesystem group:

```yaml
ai:
  tools: [filesystem]       # Prow profile only
```

No separate profile field is needed. This keeps the diagnostic contract aligned
with the tools the model can actually call. Consumer recipes still load in both
cases.

## Cache provenance

Each cache entry is stamped with the SHA-256 fingerprint of the complete merged
set in `skill_set_hash`.

The hash changes when any selected built-in or consumer recipe changes, or when
the selected profile set changes. It records which recipe contract produced an
analysis. Existing reusable entries keep their original hash and remain cached.
New analyses use the updated hash. Consumer-only whitespace and YAML comment
changes do not change the hash.

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
canonical artifact for it lives at one of these paths." Group IDs must be unique
within a recipe:

- Patterns are matched against the agent's successfully-read paths
  (full path, lowercase, slash-normalized). Use slash-style globs
  (e.g. `clusters/.*/machines/`).
- Use `any_of` to handle natural variation: different namespaces,
  different generated filenames, different controller pod names.
- Use `content_any_of` when at least one signal must appear in the selected
  artifact. Use `content_all_of` when every listed signal must appear.
- Path and content predicates are evaluated against the same artifact. Signals
  split across parallel logs do not satisfy one evidence group.
- Each regex must match one returned snippet. Snippets are never concatenated
  before matching, although different `content_all_of` predicates may match
  different snippets from the same case-sensitive artifact path.
- A bounded read, tail, or grep can prove a positive content match. A partial
  read that does not match never proves the signal is absent. The bounded repair
  tries the next ranked candidate instead.
- Use `when` only when one recipe spans distinct failure classes. Its regexes
  match the same draft text as skill triggers and keep unrelated groups out of
  critique and final validation.
- Prefer 2-3 evidence groups over a single sprawling group: smaller
  groups give more precise feedback to the model.
- Keep `description` short and human-readable; the engine surfaces
  it verbatim in critique feedback.

## Bounded critique repair

When a recipe matches and evidence is missing, the critique gate performs at
most one bounded repair operation. It injects ranked evidence first. For a
content-aware group, it tries candidates in deterministic order until one
provides positive proof or the existing artifact cap is reached. If evidence is
still unresolved, the model gets at most one batched Tool turn, followed by one
forced finalization. The general agent loop is not reopened.

`ai.critique.max_retries: 0` still evaluates recipe evidence but performs no
repair and treats critique as advisory for cache reuse. Any positive value makes
the bounded repair eligible and requires critique success, subject to context
and time-headroom guards.

## Schema versioning

Skills don't have their own schema version. Changes to a recipe change
`skill_set_hash` provenance for new analyses without invalidating existing
entries. Engine-side contract changes that make older results unacceptable, such
as a stronger deterministic critique check, bump `currentCritiqueVersion`.
Entries below that version are rejected without changing the cache-key shape.

## Authoring checklist

Before merging a new recipe:

1. **Trigger fires on a real draft.** Run `grep -i <trigger>` against
   `data/jobs/*.json` to confirm at least one analysis uses the phrase.
2. **Evidence groups match real reads.** Check the `tool_calls` of a
   matching analysis (or run a local fetch) to confirm the agent does
   fetch the artifact when prompted.
3. **Procedure is short and tool-oriented.** Quote canonical tool
   names + paths. Don't issue meta-instructions ("think carefully").
4. **Every available group resolves to canonical evidence.** A complete initial
   plan can satisfy the GCS-byte floor when every applicable group is satisfied
   or has no candidate in the complete artifact tree, and at least one group has
   a non-empty matching content read. Do not add broad patterns merely to make
   coverage easier. A candidate that exists but remains unread is still unmet.
5. **Validated before promotion.** Refetch with the recipe present
   and confirm the recipe-matched cases gain evidence reads and
   substantive root-cause depth versus the prior run.

## Observability

Bounded repair control flow is recorded in the private `ai_traces.json` file as
`critique_retry` and `critique_retry_denied` events. These events contain only
numeric and enum-like metadata, including admission, denial reason, issue
counts, evidence-read count, selected attempt, duration, and remaining time.

After the run, every `AIAnalysis` in `data/jobs/*.json` carries:

- `critique_passed`: did the final answer clear critique?
- `critique_version`: which engine contract version did it clear?
- `skill_set_hash`: fingerprint of the recipe set at the time.

Grouping analyses by `skill_set_hash` lets you compare runs before and after
a recipe or profile change.

The public manifest reports selected profiles, engine and consumer counts,
consumer bundle presence, and a short hash. Private fetch status also reports
recipe IDs and the full hash. Neither output includes procedures, trigger
patterns, evidence patterns, or recipe source text.

## Auto-suggesting recipes

The former `ai.suggest_skills` automation was removed. Consumers still author
and review recipe files under `skills/*.yaml`; their presence is the opt-in.
There is no scheduled recipe-generation feature or `SKILL_TOKEN` secret.
