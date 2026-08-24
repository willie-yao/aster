import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("mobile branding link keeps an accessible home name", () => {
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");
  const navigation = source("src/lib/navigation.ts");

  assert.match(layout, /<MuiLink[\s\S]*?aria-label=\{homeLabel\}[\s\S]*?>/);
  assert.match(
    layout,
    /<Typography[\s\S]*?display: \{ xs: "none", sm: "block" \}/,
  );
  // Both navs are rendered, so each must carry the full destination name as a
  // description. The visible label stays the accessible name so speech control
  // can target what it reads (WCAG 2.5.3).
  assert.equal(rail.match(/title=\{d\.title\}/g)?.length, 3);
  assert.doesNotMatch(rail, /aria-label=\{d\.title\}/);
  assert.match(navigation, /title: "Failure Trends"/);
  assert.match(navigation, /title: "Analysis Health"/);
  assert.match(navigation, /title: "AI Usage"/);
});

test("primary navigation swaps between a rail and a bottom bar without a gap", () => {
  const layout = source("src/components/Layout.tsx");
  const rail = source("src/components/NavRail.tsx");
  const profile = source("src/components/ProfileMenu.tsx");

  // Operator destinations render only a sign-in wall without a session, so the
  // rail must derive access from auth rather than the deployment flags alone.
  assert.match(layout, /operatorAccess: auth\.status === "authenticated"/);
  // Exactly one primary nav is visible at any width: the rail from md up, the
  // bottom bar below it.
  assert.match(rail, /display: \{ xs: "none", md: "flex" \}/);
  assert.match(rail, /display: \{ xs: "flex", md: "none" \}/);
  // The fixed bottom bar must not cover the end of the page.
  assert.match(layout, /BOTTOM_BAR_HEIGHT\}px \+ env\(safe-area-inset-bottom\)/);
  assert.match(rail, /pb: "env\(safe-area-inset-bottom\)"/);
  assert.match(layout, /href="#main-content"[\s\S]*Skip to main content/);
  assert.match(layout, /id="main-content"[\s\S]*tabIndex=\{-1\}/);
  assert.match(profile, /width: \{ xs: 44, sm: 36 \}/);
  assert.match(profile, /height: \{ xs: 44, sm: 36 \}/);
});

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /width: "100%"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowX: "auto"/);
  assert.match(timeline, /width: "max-content"[\s\S]*minWidth: "100%"/);
  assert.match(jobDetail, /<RunHistory[\s\S]*metadata=\{`\$\{historyRuns\.length\} recent/);
  assert.match(jobDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"[\s\S]*minWidth: 0/);
});

test("test analysis and run history reflow at mobile and zoom widths", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const analysis = source("src/components/AiAnalysisPanel.tsx");
  const pattern = source("src/components/PatternBanner.tsx");
  const briefing = source("src/components/AnalysisBriefing.tsx");

  assert.match(testDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"/);
  assert.match(testDetail, /display: "grid",[\s\S]*minWidth: 0/);
  assert.match(analysis, /component="section"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowWrap: "anywhere"/);
  assert.match(pattern, /<AnalysisBriefing[\s\S]*mobileSynopsis/);
  assert.doesNotMatch(pattern, /className="ai-aurora"/);
  assert.match(briefing, /component="section"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"/);
  assert.match(briefing, /mobileNotice[\s\S]*DetailSectionBand title="Full analysis"/);
  assert.match(pattern, /<CausalGroupRemediation[\s\S]*group=\{group\}/);
  assert.match(testDetail, /overflowX: "clip"/);
  assert.match(testDetail, /failure_location[\s\S]*overflowWrap: "anywhere"/);
  assert.match(timeline, /width: "100%"[\s\S]*overflowX: "auto"[\s\S]*overflowY: "hidden"/);
  assert.match(timeline, /<Tooltip title=\{tooltip\}>/);
  assert.match(timeline, /width: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /height: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /formatAccessibleDate\(run\.started\)/);
});
