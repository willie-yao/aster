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

test("AI usage coverage dividers follow the responsive grid columns", () => {
  const coverage = source("src/components/AIUsageCoverage.tsx");
  const mobileDivider = coverage.indexOf('"&:nth-of-type(even)"');
  const desktopDivider = coverage.indexOf('"&:not(:nth-of-type(3n + 1))"');

  assert.match(coverage, /"&:nth-of-type\(even\)": \{ borderInlineStart: \{ xs: "1px solid", md: 0 \}/);
  assert.match(coverage, /"&:not\(:nth-of-type\(3n \+ 1\)\)": \{ borderInlineStart: \{ md: "1px solid" \}/);
  assert.ok(mobileDivider >= 0 && mobileDivider < desktopDivider);
});

test("AI usage preserves accounting semantics in the operator-ledger composition", () => {
  const page = source("src/pages/AIUsagePage.tsx");
  const filters = source("src/components/AIUsageFilters.tsx");
  const coverage = source("src/components/AIUsageCoverage.tsx");
  const daily = source("src/components/AIUsageDaily.tsx");
  const metrics = source("src/components/MetricStrip.tsx");

  assert.match(page, /useSearchParams/);
  assert.match(page, /aiUsageFiltersFromParams/);
  assert.match(page, /setSearchParams\(aiUsageFilterParams\(values\)\)/);
  assert.match(page, /api\/ai-usage\?\$\{query\}/);
  assert.match(page, /api\/ai-usage\/download\?\$\{query\}/);
  assert.match(page, /formatRecordedUsageEstimate/);
  assert.match(page, /formatCurrentRateReprice/);
  assert.doesNotMatch(page, /current_rate_estimated_cost_nanos \?\? "0"/);
  assert.match(page, /Provider-reported tokens/);
  assert.match(page, /model requests reported usage/);
  assert.match(page, /<MetricStrip items=\{metricItems\} label="AI usage metrics"/);
  assert.match(metrics, /note\?: ReactNode/);

  assert.match(filters, /aria-expanded=\{open\}/);
  assert.match(filters, /minHeight: 48/);
  assert.match(filters, />\s*Apply\s*</);
  assert.match(filters, />\s*Reset\s*</);
  assert.match(filters, /Download JSON uses the current URL filters/);

  assert.match(coverage, /Coverage and pricing/);
  assert.match(coverage, /Provider-reported requests/);
  assert.match(coverage, /Cache-write coverage/);
  assert.match(coverage, /External unmetered/);
  assert.match(coverage, /Model gateway excluded/);
  assert.match(coverage, /About coverage and estimates/);
  assert.match(coverage, /Current-rate repricing is not actual spend/);
  assert.match(coverage, /do not form a meaningful delta/);

  assert.match(daily, /Daily cost chart/);
  assert.match(daily, /role="img"/);
  assert.match(daily, /aria-labelledby="daily-cost-chart-title"/);
  assert.match(daily, /aria-describedby="daily-cost-chart-desc daily-cost-chart-summary daily-usage-ledger-summary"/);
  assert.match(daily, /onPointerMove=\{selectPointerDay\}/);
  assert.match(daily, /onKeyDown=\{selectKeyboardDay\}/);
  assert.match(daily, /Recorded estimate \(solid\)/);
  assert.match(daily, /Current-rate estimate \(dashed\)/);
  assert.match(daily, /chartCurrencyPolicy\(recordedCurrency, currentCurrency, mixedCurrency\)/);
  assert.match(daily, /Selected-range feature mix/);
  assert.match(daily, /featureTokenPercentage\(tokens, selectedTokens\)/);
  assert.match(daily, /Daily usage ledger/);
  assert.match(daily, /aria-label="Historical daily AI usage and cost"/);
  assert.match(daily, /TableSortLabel/);
  assert.match(daily, /Partial UTC day/);
  assert.match(daily, /Not reported/);
  assert.match(daily, /FeatureBreakdown/);
  assert.match(daily, /display: \{ xs: "none", lg: "block" \}/);

  assert.doesNotMatch(page + filters + coverage + daily, /<Panel/);
  assert.doesNotMatch(page + filters + coverage + daily, /<Chip/);
  assert.doesNotMatch(page, /<Paid/);
});
