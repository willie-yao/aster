---
name: Aster
description: Evidence-first failure analysis and guarded remediation for Prow and Kubernetes test infrastructure.
colors:
  primary: "#7c3aed"
  primary-dim: "#6d28d9"
  primary-container: "#c4b5fd"
  brand-from: "#7c3aed"
  brand-to: "#ec4899"
  pass: "#1a7f37"
  pass-dim: "#116329"
  pass-container: "#dafbe1"
  warn: "#9a6700"
  warn-dim: "#7d4e00"
  warn-container: "#fff8c5"
  fail: "#cf222e"
  fail-dim: "#a40e26"
  fail-container: "#ffebe9"
  background: "#f6f8fa"
  surface: "#ffffff"
  surface-container: "#ffffff"
  surface-container-low: "#f6f8fa"
  surface-container-high: "#f0f3f6"
  surface-container-highest: "#eaeef2"
  on-surface: "#1f2328"
  on-surface-variant: "#59636e"
  outline: "#57606a"
  outline-variant: "#d0d7de"
typography:
  display:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, \"Liberation Mono\", monospace"
    fontSize: "1.75rem"
    fontWeight: 700
    lineHeight: 1.15
    letterSpacing: "-0.02em"
    fontFeature: "\"tnum\" 1"
  headline:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "27px"
    fontWeight: 700
    lineHeight: "34px"
    letterSpacing: "-0.018em"
  title:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "18px"
    fontWeight: 680
    lineHeight: "26px"
  body:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "15px"
    fontWeight: 400
    lineHeight: "22px"
  label:
    fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "13px"
    fontWeight: 700
    lineHeight: "18px"
    letterSpacing: "0"
  data:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, \"Liberation Mono\", monospace"
    fontSize: "13px"
    fontWeight: 500
    lineHeight: "19px"
    letterSpacing: "0"
    fontFeature: "\"tnum\" 1, \"cv01\" 1"
  identifier:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, \"Liberation Mono\", monospace"
    fontSize: "14px"
    fontWeight: 600
    lineHeight: "20px"
    letterSpacing: "0"
rounded:
  dot: "2px"
  default: "4px"
  overlay: "8px"
  pill: "999px"
spacing:
  hairline: "4px"
  tight: "8px"
  snug: "12px"
  default: "16px"
  section: "28px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "#ffffff"
    rounded: "{rounded.default}"
    padding: "6px 16px"
  button-primary-hover:
    backgroundColor: "{colors.primary-dim}"
  chip-pass:
    backgroundColor: "{colors.pass-container}"
    textColor: "{colors.pass}"
    rounded: "{rounded.default}"
    height: "26px"
  chip-fail:
    backgroundColor: "{colors.fail-container}"
    textColor: "{colors.fail}"
    rounded: "{rounded.default}"
    height: "26px"
  ledger-row:
    backgroundColor: "{colors.surface-container}"
    textColor: "{colors.on-surface}"
    padding: "8px 12px"
    height: "52px"
  section-band:
    backgroundColor: "{colors.surface-container-high}"
    textColor: "{colors.on-surface}"
    typography: "{typography.title}"
    height: "48px"
  input-filter:
    backgroundColor: "{colors.background}"
    textColor: "{colors.on-surface}"
    rounded: "{rounded.default}"
    height: "44px"
---

# Design System: Aster

## Overview

**Creative North Star: "The Operator's Console"**

Aster is read by a maintainer who already knows something is broken. The
interface exists to get them from a red square to a supported explanation
without hand-assembling evidence from raw artifacts. It behaves like
instrumentation rather than a document: dense, status-first, and honest about
what it does and does not know.

Density is the point, not a compromise. An overview carries dozens of jobs and
each job carries up to twenty runs, so the console is tuned for real payloads.
Chrome recedes, rules and bands do the organizing, and monospace carries every
identifier, duration, and count so numbers align into scannable columns. The
brand appears in navigation, focus, and section accents; it never touches a
pass/fail indicator.

The console is calm under load. Surfaces are flat, borders are one pixel, and
nothing floats without cause. What draws the eye is status and hierarchy, which
means an operator's attention goes where the failure is instead of where the
styling is.

**Key Characteristics:**
- Status legibility outranks expression, always.
- Tonal layering and hairline rules instead of shadows.
- Monospace for identity and measurement; system UI for prose.
- Violet chrome sits far from both pass green and fail red on the wheel.
- Every claim on screen traces to evidence the analyzer actually read.

This document was written by reading the existing implementation rather than
authored ahead of it, so it is normative going forward while the code is brought
up to it incrementally. See [Known Gaps](#known-gaps) for what does not match yet.

## Colors

A GitHub-adjacent neutral field with a violet brand ramp for chrome and a fixed
green/amber/red status vocabulary that never bends to the brand.

### Primary
- **Signal Violet** (`#7c3aed` light / `#a78bfa` dark): navigation, links, focus
  rings, section accent edges, and primary buttons. Chosen for distance from
  status hues: roughly 94 degrees from fail red and 125 degrees from pass green,
  so chrome is never mistaken for state.
- **Signal Violet Deep** (`#6d28d9` / `#c4b5fd`): hover and pressed states.
- **Violet Tint** (`#c4b5fd` / `#ddd6fe`): fills and borders only. At 1.85:1 on
  white it must never carry a label.

### Secondary
- **Pass Green** (`#1a7f37` / `#3fb950`): passing runs, healthy pass rates, and
  the pass dot.
- **Warn Amber** (`#9a6700` / `#d29922`): flaky jobs and degraded but non-fatal
  conditions.
- **Fail Red** (`#cf222e` / `#f85149`): failing runs, errors, and the fail dot.

### Tertiary
- **Brand Gradient** (`#7c3aed` → `#ec4899` light / `#a78bfa` → `#f472b6` dark):
  the mark and thin identity accents only. Never behind text.

### Neutral
- **Ink** (`#1f2328` / `#e6edf3`): body text and headings.
- **Muted Ink** (`#59636e` / `#8b949e`): secondary text, metadata, table headers.
- **Field** (`#f6f8fa` / `#0d1117`): the page behind all surfaces.
- **Surface tiers** (`#ffffff` → `#f0f3f6` → `#eaeef2` light; `#161b22` →
  `#1c2128` → `#21262d` dark): the entire depth model.
- **Rule** (`#d0d7de` / `#30363d`): every divider and border in the console.

### Named Rules

**The Status Sovereignty Rule.** Green, amber, and red mean pass, flake, and
fail. They are never restyled toward the brand, never reused decoratively, and
never the only carrier of meaning: status always has a label or text companion
beside the color, and it must survive Windows High Contrast Mode.

**The Pink Endpoint Rule.** Pink is a gradient endpoint and nothing else. At
3.53:1 on white it fails AA as text, so it is never a link, label, or button.

**The Gradient Never Reads Rule.** The brand gradient belongs to the mark and to
thin accents. It never runs behind body text, because gradient text cannot be
held to a contrast ratio.

## Typography

**Display / Body Font:** system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif
**Data / Identifier Font:** ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace

**Character:** The system UI stack keeps prose native to whatever machine the
maintainer is on, so nothing waits on a webfont before a failure is readable.
Monospace is functional, not costume: it appears wherever characters must be
compared or numbers must align, and nowhere else. The root scale is 17px, a
deliberate optical step above the 16px default that buys density without
shrinking type.

### Hierarchy
- **Stat** (mono, 700, 1.75rem / 1.15, `tnum`): the single number a metric strip
  exists to deliver.
- **Page Headline** (700, 27px / 34px, -0.018em): one per page.
- **Major Heading** (680, 18px / 26px): section bands such as Needs attention.
- **Category Heading** (680, 16px / 24px): job groups inside the ledger.
- **Subsection Heading** (700, 13.5px / 20px): bands nested inside a section.
- **Primary Body** (400, 15px / 22px): analysis prose and summaries.
- **Secondary Body** (400, 14px / 21px): supporting explanation.
- **Identifier** (mono, 600, 14px / 20px): job names and test names.
- **Description** (400, 13px / 19px): metadata and helper text.
- **Data** (mono, 500, 13px / 19px, `tnum` + `cv01`): durations, counts,
  percentages, timestamps, build IDs.
- **Table Heading** (700, 13px / 18px): ledger column headers.

### Named Rules

**The Measure Rule.** Prose caps at 65–75ch. Analysis briefings run 68ch;
denser 13px secondary blocks run 74ch. No prose block spans a full desktop
column, because a 190-character line is unreadable at any size.

**The Eleven Pixel Floor.** No functional text renders below 11px. Interactive
labels, table cells, metadata, and axis ticks all sit at or above it.

**The Sixteen Pixel Input Rule.** Text inputs render at 16px on mobile
regardless of their desktop size. iOS Safari force-zooms a focused input below
16px, which breaks the layout around it.

**The Monospace Earns It Rule.** Monospace marks code, paths, identifiers, and
measurements. It is never applied to make ordinary prose look technical.

## Layout

A fixed 76px navigation rail from `md` up, replaced below that by a 60px fixed
bottom tab bar with safe-area padding. Content sits in an `xl` container.

The console is built from full-bleed horizontal bands rather than floating
cards. A band is a header strip on a raised surface, ruled off from its body,
carrying a 3px accent edge. Rows inside a band are separated by 1px dividers,
not gaps, so long ledgers read as continuous tables instead of stacks of
containers.

Detail pages use a two-column grid at roughly 820px / 465px, where the right
column is `position: sticky; top: 80px; align-self: start` so run history and
metadata stay in view while a long analysis scrolls.

Spacing runs 4 / 8 / 12 / 16px with a 28px gap between major sections. Fixed
minimum heights hold vertical rhythm: 48px major bands, 44px category bands,
36px subsection bands, 52px ledger rows.

Breakpoints are MUI defaults plus two content-driven ones: 1024px switches the
ledger between its stacked and tabular layouts, and 1200px widens the ledger
grid. Below 1024px the ledger mounts card rows; at or above it mounts a table.
Only one of the two is ever mounted.

### Named Rules

**The One Layout Rule.** Responsive alternatives are mounted, not hidden. A
layout that is not visible at the current width does not exist in the DOM, so
the console never pays to render or reconcile a tree nobody can see.

**The Real Payload Rule.** Every layout is verified against production volumes:
dozens of jobs, twenty runs each, multi-line failure text, and job names long
enough to need truncation.

## Elevation & Depth

Flat by conviction. Shadows are noise on a console, so panels, dialogs,
tooltips, and cards set `box-shadow: none` outright. Depth comes from four
tonal surface tiers plus 1px rules: a raised band is a lighter surface with a
border, never a lifted plane.

The single exception is a true overlay. The fetch-status popover genuinely
detaches from the page and carries `0 18px 50px rgba(0, 0, 0, 0.28)`, because a
floating layer needs separation no border can provide.

### Shadow Vocabulary
- **Overlay** (`box-shadow: 0 18px 50px rgba(0, 0, 0, 0.28)`): floating popovers
  and menus only. Not for cards, bands, or dialogs.
- **Focus on color** (`box-shadow: 0 0 0 5px var(--mui-palette-background-default), 0 0 0 7px var(--mui-palette-text-primary)`):
  a double ring for focus on a saturated status square, where a violet outline
  would not survive against the fill.

### Named Rules

**The Flat Console Rule.** If a surface does not float above the page, it gets a
border and a tonal step, never a shadow.

## Shapes

Squared and industrial. The base radius is 4px and it covers nearly everything:
buttons, inputs, chips, dialogs, popovers. Status dots drop to 2px, which reads
as a machined square rather than a bubble. Only a genuine pill affordance
reaches 999px, and the floating popover takes 8px because a detached layer needs
a softer edge than an inline one.

The signature form is the accent edge: `inset 3px 0 0 currentAccent` on the
inline start of a section band. It appears 23 times and is centralized in
`sectionBandSx()`. This is a deliberate identity mark for section headers, not
a decorative border, and it is what stops a dialog from reading as a different
product than the page beneath it.

## Components

### Buttons
- **Shape:** squared, 4px radius.
- **Primary:** Signal Violet fill, white text, 600 weight, no elevation,
  `text-transform: none`.
- **Hover / Focus:** hover deepens to Signal Violet Deep; focus draws a 2px
  violet outline offset by 1px. Focus is always `:focus-visible`, never a
  suppressed outline.
- **Ghost / Icon:** transparent at rest, `surface.containerHigh` on hover, 44px
  target on mobile and 36px on desktop.
- **Font:** buttons inherit the theme family explicitly. `ButtonBase` sets no
  family of its own, so without `font: inherit` a bare button falls back to the
  user agent's Arial.

### Chips
Status chips are squared, 26px tall, 13px text at 600 weight, using a status
container fill with the matching status text color. The chip carries a word,
never a bare color.

### Ledger rows
The console's workhorse. 52px minimum height, `surface.container` fill, 1px
bottom divider, hover to `surface.containerHigh`. Focus inside a row draws
`inset 2px 0 0` on its leading edge, so keyboard position is visible without
moving anything. Rows are grid-defined with named areas so the mobile and
desktop arrangements stay declarative.

### Section bands
A raised header strip: `surface.containerHigh`, 1px rule beneath, `inset 3px 0 0`
accent edge, title on the left and metadata on the right. Every detail section
and every action dialog opens with one.

### Run history strip
A row of status squares, one per run, 8px dot in a 24x28px cell on desktop and a
44px touch cell on mobile. The strip is one composite tab stop: a single
tabbable child, arrow keys to move within it, Home and End to jump. A ledger of
28 jobs would otherwise put roughly 300 links in the tab order.

### Inputs
44px minimum height, 4px radius, `background.default` fill, divider border,
violet border on focus. 16px text on mobile, 14px from `sm` up.

### Navigation rail
76px wide, `surface.container`, 1px right rule. Each destination is an icon over
an 11px label, with the active one taking a violet tint and a 3px leading bar.
`aria-current="page"` marks only an exact URL match; a section that stays
highlighted for its nested pages reports `aria-current="true"`.

## Do's and Don'ts

**Do** carry status with a word and a color together, so it survives High
Contrast Mode and color vision deficiency.

**Do** use monospace for identifiers, durations, counts, and paths, with
`tnum` so columns align.

**Do** mount one responsive layout at a time and verify it against production
data volumes.

**Do** keep prose between 65 and 75 characters per line.

**Do** give truncated content a recovery path that works by touch and keyboard.
A `title` tooltip alone is desktop-only.

**Do** let capability absence remove an affordance cleanly. A read-only Pages
deployment shows fewer buttons, never broken ones.

**Don't** put a shadow on anything that does not genuinely float.

**Don't** restyle a pass/fail indicator toward the brand, or reuse status colors
decoratively.

**Don't** run the brand gradient behind text, or use pink as a text color.

**Don't** render functional text below 11px, or a text input below 16px on
mobile.

**Don't** add a colored left border above 1px to a card or list item. The 3px
accent edge is reserved for section bands, where it is a system signature; used
anywhere else it becomes the generic AI-callout tell.

**Don't** let a single control become one tab stop per data point. Composite
widgets get roving tabindex.

**Don't** describe Aster as autonomous repair, self-healing, or guaranteed
root-cause detection, in UI copy or anywhere else.

## Known Gaps

Aster was not built as an Impeccable project. The interface came first, and this
document was written afterward by reading the implementation: the tokens in
`frontend/src/theme/`, the components, and the rules already recorded in
`docs/brand.md`. So it is part description and part intent. It describes the
system the code already has, and it states the rules that system implies, which
the code does not yet meet everywhere.

That makes it normative going forward. New work is held to these rules, and the
bundled detector reads this file to judge changed files. Existing code is being
brought up to them incrementally rather than in one pass.

Seven rules are not yet satisfied everywhere. Each is a tracked follow-up, listed
here so the document is not mistaken for a description of what already ships.

| Rule | Current state |
| --- | --- |
| The Eleven Pixel Floor | `0.625rem` (10.6px) labels in `SearchBar`, `TestCaseTable`, `TestResultsGrid`; 10px run-history axis |
| The Sixteen Pixel Input Rule | Filter input 14px, search input 0.875rem; both force-zoom on iOS |
| The One Layout Rule | `JobHealthTable` mounts both layouts and hides one with `display: none` |
| Accent edge reserved for bands | 3px `borderLeft` on insets in `LabeledBlock` and `ChatFixDialog` |
| Composite widgets get roving tabindex | `Sparkline` puts roughly 300 run links in the overview tab order |
| `aria-current="page"` on exact match only | `NavRail` marks the active section `page` on nested routes |
| Buttons inherit the theme family | No `MuiButtonBase` override, so bare `ButtonBase` falls back to the user agent font |

The color rules above are already enforced: `docs/brand.md` and
`frontend/src/theme/tokens.ts` are the authority for those, and the interface
matches them.
