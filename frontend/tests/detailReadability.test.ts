import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("detail headings use the enlarged readable scale", () => {
  const job = source("src/pages/JobDetailPage.tsx");
  const testDetail = source("src/pages/TestDetailPage.tsx");
  const buildFailure = source("src/pages/BuildFailurePage.tsx");

  assert.match(job, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(job, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  assert.match(job, /fontWeight: 720/);
  assert.match(testDetail, /: \{ xs: "26px", sm: "30px" \}/);
  assert.match(testDetail, /: \{ xs: "33px", sm: "38px" \}/);
  assert.match(testDetail, /fontWeight: 720/);
  assert.match(buildFailure, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(buildFailure, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  assert.match(buildFailure, /fontWeight: 720/);
});

test("analysis briefing uses a calmer prose measure and rhythm", () => {
  const briefing = source("src/components/AnalysisBriefing.tsx");
  const pattern = source("src/components/PatternBanner.tsx");
  const analysis = source("src/components/AiAnalysisPanel.tsx");

  assert.match(briefing, /maxWidth: "68ch"/);
  assert.match(briefing, /fontSize: "16px"/);
  assert.match(briefing, /lineHeight: "25px"/);
  assert.match(briefing, /fontWeight: 550/);
  assert.match(briefing, /gap: 2\.25/);
  assert.match(pattern, /fontSize: "16px", lineHeight: "25px"/);
  assert.match(analysis, /fontSize: "16px", lineHeight: "25px"/);
});

test("detail strip dividers always use the quiet divider token", () => {
  const metrics = source("src/components/MetricStrip.tsx");
  const metadata = source("src/components/RunMetadata.tsx");

  assert.doesNotMatch(metrics, /borderLeft: "1px solid"/);
  assert.match(metrics, /borderInlineStartColor: "var\(--mui-palette-divider\)"/);
  assert.match(metrics, /borderTopColor: "var\(--mui-palette-divider\)"/);
  assert.doesNotMatch(metadata, /borderLeft:/);
  assert.match(metadata, /borderInlineStartColor: "var\(--mui-palette-divider\)"/);
  assert.match(metadata, /borderTopColor: "var\(--mui-palette-divider\)"/);
});

test("test rows provide a large diagnosis link and separate evidence controls", () => {
  const table = source("src/components/TestCaseTable.tsx");
  const linkStart = table.indexOf("component={RouterLink}", table.indexOf("diagnosisPath ?"));
  const linkEnd = table.indexOf("</Link>", linkStart);

  assert.notEqual(linkStart, -1);
  assert.notEqual(linkEnd, -1);
  assert.match(table, /gridColumn: \{ xs: "1", md: "1 \/ 5" \}/);
  assert.match(table, /minHeight: 54/);
  assert.match(table, /Diagnosis →/);
  assert.match(table, /gridColumn: \{ xs: "1", md: "5" \}/);
  assert.match(table, /Show inline evidence/);
  assert.match(table, /overflowX: "clip"/);
  assert.doesNotMatch(table.slice(linkStart, linkEnd), /failure_location_url/);
  assert.doesNotMatch(table, /<OpenInNew/);
});

test("AI summaries use readable body contrast instead of caption styling", () => {
  const table = source("src/components/TestCaseTable.tsx");

  assert.match(table, /tc\.ai_summary[\s\S]*component="div"/);
  assert.match(table, /color: "text\.primary"/);
  assert.match(table, /fontSize: "13\.5px"/);
  assert.match(table, /lineHeight: "20px"/);
  assert.match(table, /Likely transient/);
});
