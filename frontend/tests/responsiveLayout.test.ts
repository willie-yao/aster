import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("run history remains contained on narrow detail pages", () => {
  const timeline = source("src/components/RunTimeline.tsx");
  const jobDetail = source("src/pages/JobDetailPage.tsx");

  assert.match(timeline, /minWidth: 0, maxWidth: "100%", overflowX: "auto"/);
  assert.match(timeline, /width: "max-content", minWidth: "100%"/);
  assert.match(jobDetail, /component="section" sx=\{\{ minWidth: 0 \}\}[\s\S]*SectionHeading title="Run History"/);
  assert.match(jobDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"[\s\S]*minWidth: 0/);
});

test("test analysis and run history reflow at mobile and zoom widths", () => {
  const timeline = source("src/components/RunTimeline.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const analysis = source("src/components/AiAnalysisPanel.tsx");
  const pattern = source("src/components/PatternBanner.tsx");

  assert.match(testDetail, /gridTemplateColumns: \{ xs: "minmax\(0, 1fr\)"/);
  assert.match(testDetail, /display: "grid",[\s\S]*minWidth: 0/);
  assert.match(analysis, /component="section"[\s\S]*minWidth: 0[\s\S]*maxWidth: "100%"[\s\S]*overflowWrap: "anywhere"/);
  assert.match(pattern, /className="ai-aurora"[\s\S]*minWidth: 0[\s\S]*overflowWrap: "anywhere"/);
  assert.match(testDetail, /overflowX: "clip"/);
  assert.match(testDetail, /failure_location[\s\S]*overflowWrap: "anywhere"/);
  assert.match(timeline, /width: "100%"[\s\S]*overflowX: "auto"[\s\S]*overflowY: "hidden"/);
  assert.match(timeline, /<Tooltip title=\{tooltip\}>/);
  assert.match(timeline, /width: \{ xs: 44, sm: 40 \}/);
  assert.match(timeline, /height: \{ xs: 44, sm: 24 \}/);
  assert.match(timeline, /formatAccessibleDate\(run\.started\)/);
});
