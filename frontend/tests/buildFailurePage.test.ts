import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("standalone build failure uses the shared detail composition", () => {
  const page = source("src/pages/BuildFailurePage.tsx");

  assert.match(page, /useManifest/);
  assert.match(page, /shortJobName\([\s\S]*manifest\.short_name_prefix/);
  assert.match(page, /component="h1"[\s\S]*Build failure/);
  assert.match(page, /Failed before JUnit results/);
  assert.match(page, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(page, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  assert.match(page, /← \{displayName\}/);
  assert.match(page, /jobRunPath\(canonicalJobID, run\.build_id\)/);
  assert.match(page, /briefingTitle="Analysis briefing"/);
  assert.match(page, /mobileBriefingTitle="Analysis briefing"/);
  assert.match(page, /beforeActions=\{runMetadata\}/);
});

test("standalone build failure exposes complete run metadata with one build log link", () => {
  const page = source("src/pages/BuildFailurePage.tsx");
  const panel = source("src/components/BuildFailurePanel.tsx");

  for (const label of ["Result", "Build ID", "Started", "Finished", "Duration", "Commit"]) {
    assert.match(page, new RegExp(`label: "${label}"`));
  }
  assert.match(page, /label: "View in Prow"/);
  assert.match(page, /run\.build_log_url \? \[\{ label: "Build log"/);
  assert.equal(page.match(/label: "Build log"/g)?.length, 1);
  assert.doesNotMatch(panel, />\s*Build log/);
  assert.match(panel, /buildLogUrl: run\.build_log_url/);
});

test("build failure panel has one canonical analysis presentation", () => {
  const panel = source("src/components/BuildFailurePanel.tsx");
  const job = source("src/pages/JobDetailPage.tsx");
  const callStart = job.indexOf("<BuildFailurePanel");
  const callEnd = job.indexOf("/>", callStart);

  assert.notEqual(callStart, -1);
  assert.notEqual(callEnd, -1);
  assert.doesNotMatch(job.slice(callStart, callEnd), /appearance=/);
  assert.doesNotMatch(panel, /appearance\?: "default" \| "detail"/);
  assert.doesNotMatch(panel, /appearance = "default"/);
  assert.doesNotMatch(panel, /detailAppearance/);
  assert.doesNotMatch(panel, /from "\.\/Panel"/);
  assert.doesNotMatch(panel, /from "\.\/LabeledBlock"/);
  assert.doesNotMatch(panel, /from "\.\.\/theme"/);
  assert.doesNotMatch(panel, /<Chip|Troubleshoot|soft\(/);
  assert.match(panel, /<AnalysisBriefing/);
  assert.match(panel, /title=\{briefingTitle\}/);
  assert.match(panel, /mobileTitle=\{mobileBriefingTitle\}/);
  assert.match(panel, /source: "build" as const/);
  assert.match(panel, /<AiAnalysisPanel[\s\S]*appearance="detail"/);
  assert.match(panel, /<FailureActions[\s\S]*appearance="detail"/);
  assert.match(panel, /Open build failure details/);
  assert.match(panel, /\{stateText\[pendingState\]\.detail\}/);
  assert.doesNotMatch(panel, /failure\.ai_summary\?\.summary \?\? stateText\[pendingState\]\.detail/);
  assert.match(panel, /beforeActions[\s\S]*\{actions\}/);
  assert.match(panel, /const showActions = features\.actions/);
  assert.match(panel, /showActions && \(/);
  assert.match(panel, /const details = hasMobileDetails \?/);
});
