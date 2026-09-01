# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary user: a CI or platform maintainer triaging a shared Prow job that is
failing or flaking. They arrive already knowing something is wrong and need to
find out what, and whether it is theirs to fix. When the operator view and the
per-pull-request view compete for space, the maintainer's situation wins.

Secondary: pull request authors checking why a presubmit failed on their change,
served by the Pulls surface.

Maintainers triage from a phone as well as a desk. On-call and away-from-desk
checks are a real usage scene, not a defensive breakpoint, so a failure has to
be readable and navigable at phone width.

## Product Purpose

Aster watches Prow and TestGrid jobs, investigates failures through bounded
logs, test results, artifacts, history, and source evidence, and helps
maintainers move from signal to explanation to a reviewed next step. Success is
a maintainer reaching a supported explanation of a failure, and its next action,
without hand-assembling evidence from raw artifacts.

## Positioning

Evidence-first analysis with guarded, maintainer-controlled next steps. Every
claim is tied to artifacts the analyzer actually read, and every write action is
confirmed by a maintainer.

Aster is never described as autonomous repair, self-healing, or guaranteed
root-cause detection. Nothing files an issue or opens a pull request on a
schedule. The two scheduled GitHub write paths are opt-in and off by default:
recovery on an already-tracked issue, and a bot comment on a newly opened pull
request.

## Operating Context

Consumes Prow job configuration from kubernetes/test-infra, build artifacts and
JUnit XML from GCS, and pinned repository source. Analysis runs an agentic
tool-calling loop whose answers are gated by deterministic quality floors and a
deterministic critique pass before caching.

Two deployments read one identical JSON contract:

- GitHub Pages: static, public, read-only. No server, no admin actions.
- Kubernetes: adds authentication, chat, and guarded actions (File issue,
  Propose fix, Mark resolved).

The interface must stay correct when a capability is absent. Action affordances
appear only where the capability endpoint reports them.

## Capabilities and Constraints

- Surfaces: Overview (Needs attention, Job ledger), Failure Trends, Pulls, Job
  detail, Test detail, Build failure, plus operator-gated Analysis Health and AI
  Usage.
- Signing in unlocks the write and conversation surfaces: the inline action bar
  (Resolve failure, Draft issue, Propose fix), their dialogs and draft previews,
  bounded analysis chat, escalation, and the analysis trace ledger. These exist
  only in the Kubernetes deployment; the Pages deployment has no server and does
  not render them.
- Engine and consumer are separate repositories. Consumers own `project.yaml`
  and `prompts/system.md`; the engine owns prompts, tool schemas, cache shape,
  and the output contract.
- Pre-v1 with two internal consumers. Breaking changes are allowed anywhere and
  no backward-compatibility shims are carried.
- Data volume is real: a single job carries up to 20 recent runs, and an
  overview renders dozens of jobs at once. Layouts are sized for real payloads,
  not samples.
- Failure text is long, unpredictable, and often multi-line. Truncation must
  stay recoverable by keyboard and touch, not hover alone.

## Brand Commitments

- Aster is a proper name; its operating-model expansion is Automated Signal
  Triage, Explanation, and Remediation.
- The mark is a forward-facing prow whose counter forms a capital A, filled with
  the violet-to-pink brand gradient. No container, plate, border, or inner glyph.
- Brand and status color do different jobs. The brand ramp is violet to pink;
  status stays green, amber, and red because those carry CI meaning. A pass/fail
  indicator is never restyled to brand.
- Pink is a gradient endpoint only. At 3.53:1 on white it never becomes a link,
  label, or button color.
- The brand gradient never runs behind body text, because gradient text cannot
  be held to a contrast ratio.
- `frontend/src/theme/tokens.ts` is the authoritative source for color values;
  `docs/brand.md` mirrors it.

## Evidence on Hand

- A live deployment carrying real CAPZ Prow results, including genuine analysis
  text, causal groups, and confidence levels.
- `docs/brand.md`: mark geometry, brand ramp, interface tokens with measured
  contrast ratios, typography, and asset inventory.
- `README.md`: the source of truth for the public tagline and product
  description.
- All dashboard content is machine-generated from real runs. There is no sample,
  placeholder, or marketing copy to invent against, and none should be added.

## Product Principles

1. Evidence before conclusion. A claim the analyzer cannot support with an
   artifact it read does not belong on screen.
2. The maintainer decides. Analysis proposes; a person confirms every write.
3. Status is never decorative. Pass, flake, and fail carry meaning and are read
   before anything else on the page.
4. Density serves triage. This is an operator console, so scanability and real
   payload sizes outrank expression.
5. Absent capability degrades honestly. A read-only deployment shows fewer
   affordances, never broken or misleading ones.

## Accessibility & Inclusion

WCAG 2.1 AA is a binding target, not aspiration. Text meets 4.5:1 and large text
3:1 in both light and dark themes, and `docs/brand.md` records the measured
ratio for every interface color.

Status must never be carried by color alone: it needs a label, shape, or text
companion, and it must survive Windows High Contrast Mode.

Phone triage is a real scene, so touch targets, keyboard reachability, and
recoverable truncation are held at mobile width, not only on desktop.
