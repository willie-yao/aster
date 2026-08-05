---
goal: Redesign the Overview page as a quiet, evidence-first Kubernetes test-health operator console
version: 1.0
date_created: 2026-08-05
last_updated: 2026-08-05
owner: Frontend
status: 'Completed'
tags: [design, frontend, overview, accessibility, responsive, operator-console]
---

# Introduction

![Status: Completed](https://img.shields.io/badge/status-Completed-brightgreen)

This plan replaces the Overview page's glass, glow, pill, donut, and card-wall presentation with a dense graphite operator console. It preserves the current data contracts, filtering, navigation, search, status semantics, attention ranking, and run links.

The implementation uses focused phases. Shared visual foundations, application shell changes, summary presentation, attention rows, and job rows remain independently reviewable and revertible.

## 1. Requirements & Constraints

### Product requirements

- **REQ-001**: The Overview page must prioritize evidence, operational state, and comparison between jobs.
- **REQ-002**: The page must use neutral graphite surfaces and one restrained Azure-blue interaction accent.
- **REQ-003**: Green, amber, and red must only identify actual operational status.
- **REQ-004**: Job names, branch names, build identifiers, versions, paths, durations, and percentages must use technical monospace typography where appropriate.
- **REQ-005**: Desktop job presentation must use aligned columns rather than independent cards.
- **REQ-006**: Mobile presentation must use compact two-line rows rather than horizontally overflowing tables or miniature desktop cards.
- **REQ-007**: The page must not use backdrop blur, neon glow, gradient backgrounds, card-lift motion, or decorative animation.
- **REQ-008**: Normal panels and rows must use neutral borders rather than elevation.
- **REQ-009**: Every status represented by color must also have visible text or an accessible label.
- **REQ-010**: Long job names, descriptions, failure summaries, and branches must degrade predictably without overlapping adjacent values.

### Behavioral requirements

- **BEH-001**: Health summary items must continue to toggle the status filter.
- **BEH-002**: Selecting an active health status must continue to reset the status filter to `ALL`.
- **BEH-003**: Status and branch filters must retain their existing state semantics.
- **BEH-004**: Category order and category labels must continue to come from the manifest.
- **BEH-005**: Job links must continue to use `jobPath`.
- **BEH-006**: Recent-run links must continue to use `jobRunPath`.
- **BEH-007**: Attention links must continue to use `jobPath`, `testPath`, or `testRunPath` as currently selected.
- **BEH-008**: Recurring-pattern ranking, result limits, resolved-pattern separation, and refresh-state handling must remain unchanged.
- **BEH-009**: Search, `Cmd+K`, `Ctrl+K`, authentication, theme switching, and fetch-status behavior must remain unchanged.
- **BEH-010**: Loading, error, empty-dashboard, and no-matching-filter states must remain available.

### Technical constraints

- **CON-001**: Do not modify the backend, generated JSON contracts, or `JobSummary`.
- **CON-002**: Do not add a chart, table, animation, icon, typography, or testing dependency.
- **CON-003**: Continue using React 19, MUI 9, Emotion, and the existing Node test runner.
- **CON-004**: Preserve dark and light mode.
- **CON-005**: Preserve the same-origin font and script security policy.
- **CON-006**: Do not load Google Fonts or external design assets.
- **CON-007**: Use existing MUI icons unless a new icon is strictly necessary.
- **CON-008**: Do not redesign unrelated detail pages as part of the Overview work.
- **CON-009**: Shared component changes must be checked on every existing caller before merge.
- **CON-010**: Deployment is outside this implementation plan and requires separate authorization.

### Accessibility and performance requirements

- **A11Y-001**: Normal text must meet WCAG AA contrast.
- **A11Y-002**: Interactive elements must have a visible 2px Azure-blue focus indicator.
- **A11Y-003**: Mobile controls must provide at least a 44px effective touch target.
- **A11Y-004**: Heading order must remain `h1`, then category or panel `h2`, then row-level headings where appropriate.
- **A11Y-005**: The new job rows must not contain nested anchors or nested buttons.
- **A11Y-006**: Hidden responsive content must use `display: none` rather than visually clipping duplicate focusable content.
- **PERF-001**: Remove backdrop-filter work from Overview surfaces.
- **PERF-002**: Do not introduce a visualization library for the health summary.
- **PERF-003**: Preserve the current bounded eight-run rendering in `Sparkline`.

## 2. Implementation Steps

### Implementation Phase 1

- **GOAL-001**: Establish operator-console tokens.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-001 | Update dark and light graphite palette values in `frontend/src/theme/tokens.ts`. | ✅ | 2026-08-05 |
| TASK-002 | Update the typography scale and base radius in `frontend/src/theme/createAppTheme.ts`. | ✅ | 2026-08-05 |
| TASK-003 | Remove the root 17px scaling exception in `frontend/src/index.css`. | ✅ | 2026-08-05 |
| TASK-004 | Add theme contract tests and register them in `frontend/tests/run.ts`. | ✅ | 2026-08-05 |

### Implementation Phase 2

- **GOAL-002**: Simplify shared surfaces and status treatment.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-005 | Make `Panel` opaque, border-defined, and shadow-free. | ✅ | 2026-08-05 |
| TASK-006 | Remove the obsolete `surface.glass` theme key after migrating callers. | ✅ | 2026-08-05 |
| TASK-007 | Restyle `StatusChip` without glow or full pill geometry. | ✅ | 2026-08-05 |
| TASK-008 | Replace rounded section ticks with a square Azure-blue rule. | ✅ | 2026-08-05 |
| TASK-009 | Simplify `Sparkline` motion and marker sizing while preserving links. | ✅ | 2026-08-05 |
| TASK-010 | Remove glow generation from the temporary `statusAccent` helper. | ✅ | 2026-08-05 |

### Implementation Phase 3

- **GOAL-003**: Quiet the application shell.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-011 | Make the AppBar opaque with a neutral bottom border. | ✅ | 2026-08-05 |
| TASK-012 | Remove the brand tile glow and reduce its radius. | ✅ | 2026-08-05 |
| TASK-013 | Replace navigation pills with text navigation and an active underline. | ✅ | 2026-08-05 |
| TASK-014 | Preserve mobile navigation touch targets and horizontal access. | ✅ | 2026-08-05 |
| TASK-015 | Restyle search surfaces without changing search behavior. | ✅ | 2026-08-05 |

### Implementation Phase 4

- **GOAL-004**: Structure Overview controls and the health summary.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-016 | Add `frontend/src/components/OverviewFilters.tsx`. | ✅ | 2026-08-05 |
| TASK-017 | Render status as a compact segmented control and branch as a select. | ✅ | 2026-08-05 |
| TASK-018 | Refactor `HealthPanel` into aligned total and status values. | ✅ | 2026-08-05 |
| TASK-019 | Preserve status-filter toggle behavior and pressed state. | ✅ | 2026-08-05 |
| TASK-020 | Remove the fixed summary height and decorative updated glow. | ✅ | 2026-08-05 |
| TASK-021 | Delete `HealthDonut.tsx` after caller migration. | ✅ | 2026-08-05 |
| TASK-022 | Add Overview presentation tests. | ✅ | 2026-08-05 |

### Implementation Phase 5

- **GOAL-005**: Convert Needs Attention into aligned evidence rows.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-023 | Refactor `NeedsAttention.tsx` without changing ranking or routing. | ✅ | 2026-08-05 |
| TASK-024 | Add a local `AttentionRow` presentation helper. | ✅ | 2026-08-05 |
| TASK-025 | Align subject, evidence, count, and confidence on desktop. | ✅ | 2026-08-05 |
| TASK-026 | Keep confidence and build counts neutral. | ✅ | 2026-08-05 |
| TASK-027 | Remove internal vertical scrolling. | ✅ | 2026-08-05 |
| TASK-028 | Reflow rows without overlap below 768px. | ✅ | 2026-08-05 |
| TASK-029 | Preserve resolved-pattern disclosure and all-clear handling. | ✅ | 2026-08-05 |

### Implementation Phase 6

- **GOAL-006**: Replace job cards with a grouped job-health ledger.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-030 | Add `frontend/src/components/JobHealthTable.tsx`. | ✅ | 2026-08-05 |
| TASK-031 | Align Job, Branch, Recent runs, Pass rate, Last run, Duration, and Status. | ✅ | 2026-08-05 |
| TASK-032 | Use monospace identifiers and tabular metric values. | ✅ | 2026-08-05 |
| TASK-033 | Preserve `Sparkline` run links and tooltips. | ✅ | 2026-08-05 |
| TASK-034 | Keep job and run links as separate valid anchors. | ✅ | 2026-08-05 |
| TASK-035 | Add medium and mobile row layouts without horizontal overflow. | ✅ | 2026-08-05 |
| TASK-036 | Replace both `JobCard` render paths in `DashboardPage.tsx`. | ✅ | 2026-08-05 |
| TASK-037 | Delete `JobCard.tsx` and the dead `statusAccent` helper. | ✅ | 2026-08-05 |
| TASK-038 | Add row route, semantics, and nesting tests. | ✅ | 2026-08-05 |

### Implementation Phase 7

- **GOAL-007**: Complete responsive, accessibility, and cleanup validation.

| Task | Description | Completed | Date |
|------|-------------|-----------|------|
| TASK-039 | Test 390, 768, 1024, and 1440 widths in dark and light mode. | ✅ | 2026-08-05 |
| TASK-040 | Verify no page-level horizontal overflow or internal vertical panel scrolling. | ✅ | 2026-08-05 |
| TASK-041 | Keyboard-test all Overview and shell controls. | ✅ | 2026-08-05 |
| TASK-042 | Verify visible focus and non-color status communication. | ✅ | 2026-08-05 |
| TASK-043 | Remove dead glass, glow, donut, card, and lift references. | ✅ | 2026-08-05 |
| TASK-044 | Run frontend tests, lint, type-check, and build. | ✅ | 2026-08-05 |
| TASK-045 | Capture final screenshots and compare information presence against the baseline. | ✅ | 2026-08-05 |

## 3. Alternatives

- **ALT-001**: Complete the redesign in one PR. Rejected because theme, shell, attention rows, and job presentation have different regression surfaces.
- **ALT-002**: Only restyle the existing job cards. Rejected because cards cannot provide stable cross-row comparison or tabular alignment.
- **ALT-003**: Add MUI DataGrid. Rejected because it adds dependency and interaction complexity that the current read-only list does not need.
- **ALT-004**: Use a horizontally scrolling native table on mobile. Rejected because horizontal navigation makes routine triage slower.
- **ALT-005**: Retain a smaller donut. Rejected because aligned values communicate the same information more efficiently.
- **ALT-006**: Introduce a web font. Rejected because the system stack is appropriate and avoids CSP and performance costs.

## 4. Dependencies

- **DEP-001**: Existing MUI theme and CSS-variable support.
- **DEP-002**: Existing dashboard and pattern data contracts.
- **DEP-003**: Existing route helpers in `frontend/src/lib/routes.ts`.
- **DEP-004**: Existing manifest category helpers.
- **DEP-005**: Existing Node test runner and Vite SSR test pattern.
- **DEP-006**: Existing production data for visual verification.

## 5. Files

### Files expected to be modified

- `frontend/src/theme/tokens.ts`
- `frontend/src/theme/createAppTheme.ts`
- `frontend/src/theme/components.ts`
- `frontend/src/theme/augmentation.ts`
- `frontend/src/theme/helpers.ts`
- `frontend/src/components/Panel.tsx`
- `frontend/src/components/Layout.tsx`
- `frontend/src/components/SearchBar.tsx`
- `frontend/src/components/StatusChip.tsx`
- `frontend/src/components/SectionHeading.tsx`
- `frontend/src/components/Sparkline.tsx`
- `frontend/src/components/HealthPanel.tsx`
- `frontend/src/components/FetchStatus.tsx`
- `frontend/src/components/NeedsAttention.tsx`
- `frontend/src/pages/DashboardPage.tsx`
- `frontend/src/index.css`
- `frontend/tests/run.ts`

### Files expected to be added

- `frontend/src/components/OverviewFilters.tsx`
- `frontend/src/components/JobHealthTable.tsx`
- `frontend/src/lib/dashboardOverview.ts`
- `frontend/tests/theme.test.ts`
- `frontend/tests/dashboardOverview.test.ts`

### Files expected to be deleted after caller migration

- `frontend/src/components/HealthDonut.tsx`
- `frontend/src/components/JobCard.tsx`

## 6. Testing

- **TEST-001**: Theme tokens expose the approved colors and base radius.
- **TEST-002**: Status presentation includes visible text and does not depend only on color.
- **TEST-003**: Health summary preserves counts, percentages, filter labels, and pressed state.
- **TEST-004**: Branch options preserve existing sorting.
- **TEST-005**: Attention rows preserve canonical routing and resolved disclosure semantics.
- **TEST-006**: Job rows preserve job and recent-run routes.
- **TEST-007**: Job rows do not render nested anchors or nested buttons.
- **TEST-008**: Technical identifiers render through the monospace typography variant.
- **TEST-009**: Existing search, route, fetch-status, flakiness, and security tests remain green.
- **TEST-010**: Production build verification remains green.

Required commands:

```bash
cd frontend
npm test
npm run lint
npx tsc -b
npm run build
```

## 7. Risks & Assumptions

- **RISK-001**: Global typography and radius changes can affect detail pages. Mitigation: inspect all major surfaces in both schemes.
- **RISK-002**: Removing `Panel` blur affects many callers. Mitigation: keep the primitive change independently reviewable.
- **RISK-003**: Long CAPZ names can pressure medium-width rows. Mitigation: use `minmax(0, ...)`, ellipsis, and full accessible names.
- **RISK-004**: Mobile reflow can hide useful context. Mitigation: never hide status, branch, pass rate, or last run.
- **RISK-005**: Replacing whole-card navigation reduces the clickable area. Mitigation: provide a clear job link while preserving separate run links.
- **RISK-006**: Light-mode status colors may fail contrast. Mitigation: test every status treatment in both schemes.
- **ASSUMPTION-001**: Structural redesign remains limited to Overview and the shared shell.
- **ASSUMPTION-002**: Other pages may inherit token and primitive changes but do not receive structural redesigns.
- **ASSUMPTION-003**: Deployment remains outside this plan.

## 8. Related Specifications / Further Reading

- `AGENTS.md`
- `frontend/src/pages/DashboardPage.tsx`
- `frontend/src/theme/createAppTheme.ts`
- `frontend/src/theme/components.ts`
- `frontend/src/components/TestCaseTable.tsx`
- `frontend/tests/flakinessPage.test.ts`
- `frontend/tests/search.test.ts`
