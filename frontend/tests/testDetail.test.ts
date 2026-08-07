import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("test detail uses parsed titles with a protected canonical fallback", () => {
  const page = source("src/pages/TestDetailPage.tsx");

  assert.match(page, /parseTestDisplayName\(testName\)/);
  assert.match(page, /parsedTitle\.displayName/);
  assert.match(page, /aria-label=\{parsedTitle\.usedFallback \? testName : undefined\}/);
  assert.match(page, /fontSize: parsedTitle\.usedFallback/);
  assert.match(page, /WebkitLineClamp: parsedTitle\.usedFallback \? \{ xs: 3, sm: 2 \} : undefined/);
  assert.match(page, /title=\{testName\}/);
});

test("test detail preserves technical identity and selected-run routing", () => {
  const page = source("src/pages/TestDetailPage.tsx");

  assert.match(page, /<TechnicalIdentity/);
  assert.match(page, /Canonical test name/);
  assert.match(page, /Structured labels/);
  assert.match(page, /Removed suite prefix/);
  assert.match(page, /parsedTitle\.removedPrefixes\.join\(" · "\)/);
  assert.match(page, /withJobDetailParam\(searchParams, "run", buildID\)/);
  assert.match(page, /<RunHistory[\s\S]*onSelect=\{selectRun\}/);
  assert.match(page, /<RunMetadata/);
  assert.match(page, /← \{jobDisplayName\}/);
});

test("test detail uses the approved analysis and evidence composition", () => {
  const page = source("src/pages/TestDetailPage.tsx");

  assert.match(page, /<MetricStrip items=\{metricItems\} label="Test metrics"/);
  assert.match(page, /<AnalysisBriefing/);
  assert.match(page, /icon=\{<AutoAwesome/);
  assert.match(page, /collapseDetailsOnMobile=\{false\}/);
  assert.match(page, /<AiAnalysisPanel[\s\S]*appearance="detail"/);
  assert.match(page, /traceHref=\{traceHref\}/);
  assert.match(page, /fixPatterns=\{fixPatterns\}/);
  assert.match(page, /chatRef=\{\{/);
  assert.match(page, /title="Stack trace"/);
  assert.match(page, /aria-expanded=\{stackOpen\}/);
  assert.match(page, /stackOpenFor === effectiveSelectedID/);
  assert.match(page, /title="Files and evidence"/);
  assert.match(page, /Failure location/);
  assert.match(page, /JUnit artifact/);
  assert.match(page, /Provider activity/);
  assert.match(page, /Machine logs/);
  assert.match(page, /View in Prow/);
  assert.match(page, /Build log/);

  assert.doesNotMatch(page, /<Panel/);
  assert.doesNotMatch(page, /<StatusChip/);
  assert.doesNotMatch(page, /ai-aurora/);
});
