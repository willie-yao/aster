import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { summarizeTestHistory } from "../src/lib/testDetail.js";
import { DEFAULT_PERSISTENT_AFTER, persistentAfter } from "../src/lib/attention.js";
import type { Manifest } from "../src/types/manifest.js";
import type { TestCase } from "../src/types/dashboard.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}


function historyCase(status: TestCase["status"] | null) {
  return {
    testCase: status
      ? { name: status, status, duration_seconds: 0 }
      : null,
  };
}

const historyCases = [
  {
    name: "skipped and absent do not break persistence",
    statuses: ["failed", "skipped", null, "failed", "failed"] as const,
    failureRate: 1,
    consecutiveFailures: 3,
    classification: "Persistent (3×)",
  },
  {
    name: "executed results alone determine flaky rate",
    statuses: ["failed", "skipped", "passed", null, "failed"] as const,
    failureRate: 2 / 3,
    consecutiveFailures: 1,
    classification: "Flaky",
  },
  {
    name: "skipped-only history has no rate",
    statuses: ["skipped", null, "skipped"] as const,
    failureRate: null,
    consecutiveFailures: 0,
    classification: null,
  },
];

for (const testCase of historyCases) {
  test(`test history: ${testCase.name}`, () => {
    const summary = summarizeTestHistory(
      testCase.statuses.map((status) => historyCase(status)),
      DEFAULT_PERSISTENT_AFTER,
    );
    assert.equal(summary.failureRate, testCase.failureRate);
    assert.equal(summary.consecutiveFailures, testCase.consecutiveFailures);
    assert.equal(summary.classification, testCase.classification);
  });
}

// A configured attention.persistent_after must move the client-side
// classification too, or Test Detail contradicts the published one.
test("test history classification follows the configured persistent threshold", () => {
  const twoFailures = ["passed", "failed", "failed"].map((status) =>
    historyCase(status as "failed" | "passed"),
  );
  assert.equal(summarizeTestHistory(twoFailures, DEFAULT_PERSISTENT_AFTER).classification, "Flaky");
  assert.equal(summarizeTestHistory(twoFailures, 2).classification, "Persistent (2×)");

  const threeFailures = ["failed", "failed", "failed"].map((status) =>
    historyCase(status as "failed"),
  );
  assert.equal(
    summarizeTestHistory(threeFailures, DEFAULT_PERSISTENT_AFTER).classification,
    "Persistent (3×)",
  );
  // Below the threshold an all-failure history is one-off, matching the
  // backend's classifyOutcomes rather than going unclassified.
  assert.equal(summarizeTestHistory(threeFailures, 5).classification, "One-off failure");
});

test("persistentAfter resolves the manifest value with the engine default", () => {
  const base = { id: "x", name: "x" } as Manifest;
  assert.equal(persistentAfter(base), DEFAULT_PERSISTENT_AFTER);
  assert.equal(persistentAfter({ ...base, attention: {} } as Manifest), DEFAULT_PERSISTENT_AFTER);
  assert.equal(
    persistentAfter({ ...base, attention: { persistent_after: 0 } } as Manifest),
    DEFAULT_PERSISTENT_AFTER,
  );
  assert.equal(persistentAfter({ ...base, attention: { persistent_after: 5 } } as Manifest), 5);
});

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
  assert.match(page, /label: "Median duration"/);
  assert.match(page, /label: "95th percentile"/);
  assert.match(page, /\{runHistory\}[\s\S]*<RuntimeTrend[\s\S]*summary=\{runtimeSummary\}[\s\S]*\{runMetadata\}/);
  assert.match(page, /const runMetadata = selectedRun \?/);
  assert.match(page, /value: selectedTestCase[\s\S]*: "Not present"/);
  assert.match(page, /View in Prow/);
  assert.match(page, /Build log/);
  assert.match(page, /← \{jobDisplayName\}/);
});

test("test detail uses the approved analysis and evidence composition", () => {
  const page = source("src/pages/TestDetailPage.tsx");

  assert.match(page, /<MetricStrip items=\{metricItems\} label="Test metrics"/);
  assert.match(page, /<AnalysisBriefing/);
  assert.match(page, /icon=\{<AutoAwesome/);
  assert.doesNotMatch(page, /collapseDetailsOnMobile=\{false\}/);
  assert.match(page, /mobileNotice=\{selectedTestCase\.ai_analysis\.disposition === "preliminary"/);
  assert.match(page, /<AiAnalysisPanel[\s\S]*appearance="detail"/);
  assert.match(page, /traceRef=\{traceRef\}/);
  assert.match(page, /fixPatterns=\{fixPatterns\}/);
  assert.match(page, /patternLifecycleActive\(pattern\.lifecycle\)/);
  assert.match(page, /chatRef=\{\{/);
  assert.match(page, /title="Stack trace"/);
  assert.match(page, /aria-expanded=\{stackOpen\}/);
  assert.match(page, /stackOpenFor === effectiveSelectedID/);
  assert.match(page, /title="Files and evidence"/);
  assert.match(page, /Failure location/);
  assert.match(page, /JUnit artifact/);
  assert.match(page, /Provider activity/);
  assert.match(page, /controllerLogsFallback/);
  assert.match(page, /artifacts\/clusters\/bootstrap\/logs\//);
  assert.match(page, /Controller logs/);
  assert.match(page, /Machine logs/);
  assert.match(page, /View in Prow/);
  assert.match(page, /Build log/);

  assert.doesNotMatch(page, /<Panel/);
  assert.doesNotMatch(page, /<StatusChip/);
  assert.doesNotMatch(page, /ai-aurora/);
});
