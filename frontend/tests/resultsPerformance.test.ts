import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  sortResultTests,
  summarizeResultTests,
} from "../src/lib/jobDetail.js";
import {
  initialProgressiveCount,
  nextProgressiveCount,
  trailingWindowStart,
} from "../src/lib/progressive.js";
import {
  gridCellAccessibleName,
  gridStatusSymbol,
} from "../src/lib/testResultsGrid.js";
import type { TestCase } from "../src/types/dashboard.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

function testCase(name: string, status: TestCase["status"]): TestCase {
  return { name, status, duration_seconds: 1 };
}

test("result summaries account for hidden successful setup and teardown cases", () => {
  const summary = summarizeResultTests([
    testCase("passes", "passed"),
    testCase("fails", "failed"),
    testCase("BeforeSuite", "passed"),
    testCase("AfterSuite", "failed"),
    testCase("skipped", "skipped"),
  ]);

  assert.equal(summary.executed.length, 4);
  assert.equal(summary.hiddenSuccessfulSetupTeardown, 1);
  assert.deepEqual(summary.visible.map((item) => item.name), ["passes", "fails", "AfterSuite"]);
  assert.deepEqual(
    sortResultTests(summary.visible).map((item) => item.name),
    ["fails", "AfterSuite", "passes"],
  );
});

test("progressive windows cap initial rendering and expand in bounded batches", () => {
  assert.equal(initialProgressiveCount(442, 50), 50);
  assert.equal(nextProgressiveCount(50, 442, 50), 100);
  assert.equal(nextProgressiveCount(400, 442, 50), 442);
  assert.equal(trailingWindowStart(20, 12), 8);
  assert.equal(trailingWindowStart(8, 12), 0);
});

test("cross-run cells expose test, run, date, and status without relying on color", () => {
  assert.equal(
    gridCellAccessibleName(
      "Conformance Tests should pass",
      "2084380319225483264",
      "2026-08-07T12:00:00Z",
      "failed",
    ),
    "Conformance Tests should pass, run 2084380319225483264 on Aug 7, 2026, failed",
  );
  assert.equal(gridStatusSymbol("passed"), "✓");
  assert.equal(gridStatusSymbol("failed"), "×");
  assert.equal(gridStatusSymbol("skipped"), "–");
});

test("result views progressively render rows and grid cells", () => {
  const page = source("src/pages/JobDetailPage.tsx");
  const grid = source("src/components/TestResultsGrid.tsx");
  const ledger = source("src/components/ResultLedger.tsx");

  assert.match(page, /const resultBatchSize = 50/);
  assert.match(page, /visibleTestCases\.slice\(0, renderedResultCount\)/);
  assert.match(page, /hiddenSuccessfulSetupTeardown=\{resultSummary\.hiddenSuccessfulSetupTeardown\}/);
  assert.match(grid, /const rowBatchSize = 50/);
  assert.match(grid, /const runBatchSize = 12/);
  assert.match(grid, /const visibleRows = gridRows\.slice\(0, visibleRowCount\)/);
  assert.match(grid, /const visibleRuns = sortedRuns\.slice\(runStart\)/);
  assert.match(grid, /aria-label=\{label\}/);
  assert.match(grid, /gridStatusSymbol\(status\)/);
  assert.match(ledger, /successful setup\/teardown hidden/);
  assert.match(ledger, /All statuses/);
  assert.match(ledger, /showMoreCount/);
});

test("AI usage labels cost and token coverage accurately", () => {
  const page = source("src/pages/AIUsagePage.tsx");

  assert.match(page, /Estimated cost for priced records/);
  assert.match(page, /Estimated cost covers priced records only/);
  assert.match(page, /Provider-reported tokens/);
  assert.match(page, /priced_reported_requests/);
  assert.match(page, /model requests reported usage/);
  assert.match(page, /Historical daily cost/);
  assert.match(page, /Recorded estimates use the price stored with each operation/);
  assert.match(page, /Current-rate estimates apply the rates configured now/);
  assert.match(page, /aria-label="Historical daily AI usage and cost"/);
  assert.match(page, /role="img"/);
  assert.match(page, /aria-labelledby="daily-cost-chart-title daily-cost-chart-desc"/);
  assert.match(page, /onPointerMove=\{selectPointerDay\}/);
  assert.match(page, /preserveAspectRatio="xMidYMid meet"/);
  assert.match(page, /chartViewBoxLayout\(bounds\.width, bounds\.height, width, height\)/);
  assert.match(page, /chartViewBoxPoint\(event\.clientX - bounds\.left, event\.clientY - bounds\.top, layout\)/);
  assert.match(page, /onKeyDown=\{selectKeyboardDay\}/);
  assert.match(page, /Recorded estimate \(solid\)/);
  assert.match(page, /Current-rate estimate \(dashed\)/);
  assert.match(page, /\{recordedPath && <Typography/);
  assert.match(page, /\{currentPath && <Typography/);
  assert.match(page, /role="status" aria-live="polite"/);
  assert.match(page, /Coverage: \{activeDay\.coverage\.status\}/);
  assert.match(page, /var\(--mui-palette-warning-main\)/);
  assert.match(page, /chartScale\(rawMax, availableIndexes\.length > 0\)/);
  assert.match(page, /chartCurrencyPolicy\(recordedCurrency, currentCurrency, mixedCurrency\)/);
  assert.match(page, /chartSeriesDescription\(Boolean\(recordedPath\), Boolean\(currentPath\)\)/);
  assert.match(page, /chartDateTickIndexes\(days\.length\)/);
  assert.doesNotMatch(page, /`\$\{exact\} partial`/);
  assert.match(page, /Partial UTC day/);
  assert.match(page, /No usage recorded/);
  assert.match(page, /TableSortLabel/);
  assert.match(page, /FeatureBreakdown/);
  for (const mobileMetric of ["Cache hits", "Uncached input", "Cached read", "Cache write", "Output"]) {
    assert.match(page, new RegExp(`>${mobileMetric}<`));
  }
  assert.match(page, /Not reported/);
  assert.match(page, /overflowX: "auto"/);
  assert.match(page, /minWidth: 200/);
});
