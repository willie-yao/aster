import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  chartCurrencyPolicy, chartDateTickIndexes, chartScale, chartSeriesDescription,
  chartTickValues, formatChartCost, formatCost, formatCoverage, formatExactCost,
  formatExactTokens, formatTokens, nearestChartDataIndex,
  pricedRequestCoverageNote, totalTokens, uncachedInputTokens, usageQuery,
} from "../src/lib/aiUsage.js";
import type { AIUsageReport, AIUsageTotals } from "../src/types/usage.js";

test("AI usage helpers format values and filters", () => {
  assert.equal(formatTokens(1200), "1,200");
  assert.equal(formatCost("1250000000", "USD"), "$1.25");
  assert.equal(formatExactCost("1250000001", "USD"), "USD 1.25");
  assert.equal(formatExactCost("1255000000", "USD"), "USD 1.26");
  assert.equal(formatChartCost(23.1, "USD"), "$23.10");
  assert.equal(formatChartCost(23.1), "23.10");
  assert.deepEqual(chartTickValues(50.28), [0, 20, 40, 60]);
  assert.deepEqual(chartDateTickIndexes(30), [0, 6, 12, 17, 23, 29]);
  assert.equal(nearestChartDataIndex(7, [0, 5, 8, 12]), 8);
  assert.equal(formatExactTokens(1234567), "1,234,567");
  assert.equal(totalTokens({ input_tokens: 2, output_tokens: 3 } as never), 5);
  assert.equal(uncachedInputTokens({ input_tokens: 20, cached_input_tokens: 5, cache_write_input_tokens: 3 } as never), 12);
  assert.equal(formatCoverage(7, 10), "7 of 10 (70%)");
  assert.equal(usageQuery("2026-08-01", "2026-08-03", "analysis_chat"), "start=2026-08-01&end=2026-08-03&feature=analysis_chat");
});

test("daily cost chart preserves zero values and separates currencies", () => {
  assert.deepEqual(chartScale(0, false), { ticks: [], max: 0 });
  assert.deepEqual(chartScale(0, true), { ticks: [0, 0.01], max: 0.01 });
  assert.deepEqual(chartScale(50.28, true), { ticks: [0, 20, 40, 60], max: 60 });
  assert.deepEqual(chartScale(0.001, true), { ticks: [0, 0.01], max: 0.01 });
  assert.deepEqual(chartScale(0.005, true), { ticks: [0, 0.01], max: 0.01 });
  assert.deepEqual(chartScale(0.009, true), { ticks: [0, 0.01], max: 0.01 });
  assert.deepEqual(chartScale(0.011, true), { ticks: [0, 0.01, 0.02], max: 0.02 });
  assert.deepEqual(chartCurrencyPolicy("USD", "EUR"), {
    showRecorded: true,
    showCurrent: false,
    note: "Current-rate series omitted because current rates use EUR while recorded estimates use USD.",
  });
  assert.deepEqual(chartCurrencyPolicy("USD", "USD"), { showRecorded: true, showCurrent: true });
  assert.deepEqual(chartCurrencyPolicy("USD", "USD", true), {
    showRecorded: false,
    showCurrent: true,
    note: "Recorded series omitted because recorded estimates contain multiple currencies.",
  });
});

test("daily cost chart descriptions match visible series", () => {
  assert.match(chartSeriesDescription(true, false), /^Solid blue shows recorded estimates\./);
  assert.doesNotMatch(chartSeriesDescription(true, false), /current-rate/);
  assert.match(chartSeriesDescription(false, true), /^Dashed amber shows current-rate estimates\./);
  assert.doesNotMatch(chartSeriesDescription(false, true), /recorded estimates/);
  assert.match(chartSeriesDescription(true, true), /Solid blue.*Dashed amber/);
});

test("legacy AI usage reports tolerate omitted priced request counts", () => {
  const legacyTotals: AIUsageTotals = {
    operations: 1, cache_hits: 0, failures: 0, external_unmetered_operations: 0,
    model_requests: 7, reported_requests: 7, unreported_requests: 0,
    input_tokens: 100, cached_input_tokens: 0, output_tokens: 20, reasoning_tokens: 0,
    estimated_cost_nanos: "1000",
  };
  const legacyCoverage: AIUsageReport["coverage"] = {
    status: "partial", model_requests: 7, reported_requests: 7,
    unreported_requests: 0, external_unmetered_operations: 0,
  };
  assert.equal(legacyTotals.priced_reported_requests, undefined);
  assert.equal(legacyCoverage.priced_reported_requests, undefined);
  assert.equal(pricedRequestCoverageNote(legacyTotals.priced_reported_requests ?? legacyCoverage.priced_reported_requests, legacyTotals.reported_requests, "partial"), "Priced-request coverage is unavailable for legacy records");
  assert.equal(pricedRequestCoverageNote(4, 7, "partial"), "4 of 7 (57%) reported requests priced");
});

test("historical usage fixture covers required cost and coverage scenarios", () => {
  const fixture = JSON.parse(readFileSync(resolve(process.cwd(), "tests/fixtures/ai-usage-history.json"), "utf8")) as { daily: Array<{ scenario: string; current_partial_utc: boolean; totals: { cache_hits: number; cache_write_input_tokens: number }; coverage: { states: string[] } }> };
  const scenarios = new Set(fixture.daily.map((day) => day.scenario));
  for (const scenario of ["high cold analysis", "mostly warm cache", "pattern failures", "pricing unavailable", "external unmetered", "cache write tokens", "partial current day"]) {
    assert.ok(scenarios.has(scenario), `missing ${scenario}`);
  }
  assert.ok(fixture.daily.some((day) => day.totals.cache_hits > 0));
  assert.ok(fixture.daily.some((day) => day.totals.cache_write_input_tokens > 0));
  assert.ok(fixture.daily.some((day) => day.coverage.states.includes("external_unmetered")));
  assert.ok(fixture.daily.some((day) => day.current_partial_utc));
});
