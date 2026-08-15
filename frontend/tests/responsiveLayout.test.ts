import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("mobile branding link keeps an accessible home name", () => {
  const layout = source("src/components/Layout.tsx");

  assert.match(
    layout,
    /<MuiLink[\s\S]*?aria-label=\{`\$\{manifest\.branding\.title\} home`\}[\s\S]*?>/,
  );
  assert.match(
    layout,
    /<Typography[\s\S]*?display: \{ xs: "none", sm: "block" \}/,
  );
  assert.match(layout, /label="Failure Trends"/);
  assert.match(layout, /label="Analysis Traces"/);
  assert.match(layout, /label="AI Usage"/);
});

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunHistory.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /width: "100%"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowX: "auto"/);
  assert.match(timeline, /width: "max-content"[\s\S]*minWidth: "100%"/);
  assert.match(jobDetail, /<RunHistory[\s\S]*metadata=\{`\$\{runs\.length\} recent/);
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
  assert.match(briefing, /mobileNotice[\s\S]*followUp[\s\S]*DetailSectionBand title="Full analysis"/);
  assert.match(pattern, /followUp=\{analysisOnly/);
  assert.match(testDetail, /overflowX: "clip"/);
  assert.match(testDetail, /failure_location[\s\S]*overflowWrap: "anywhere"/);
  assert.match(timeline, /width: "100%"[\s\S]*overflowX: "auto"[\s\S]*overflowY: "hidden"/);
  assert.match(timeline, /<Tooltip title=\{tooltip\}>/);
  assert.match(timeline, /width: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /height: \{ xs: 44, sm: 32 \}/);
  assert.match(timeline, /formatAccessibleDate\(run\.started\)/);
});
