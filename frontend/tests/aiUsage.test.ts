import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  aiUsageFilterParams, aiUsageFilterSummary, aiUsageFiltersAreCustom, aiUsageFiltersFromParams,
  compactAIUsageFilterSummary,
  chartCurrencyPolicy, chartDateTickIndexes, chartScale, chartSeriesDescription, chartViewBoxLayout,
  chartViewBoxPoint, chartViewportX,
  chartTickValues, coverageStateLabel, defaultAIUsageFilters, featureTokenPercentage,
  formatChartCost, formatCost, formatCoverage, formatCurrentRateReprice, formatExactCost,
  formatExactTokens, formatTokens, nearestChartDataIndex,
  formatRecordedUsageEstimate, pricedRequestCoverageNote, totalTokens, uncachedInputTokens, usageQuery,
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
  const defaults = defaultAIUsageFilters(new Date("2026-08-11T12:00:00Z"));
  assert.deepEqual(defaults, { start: "2026-07-13", end: "2026-08-11", feature: "" });
  const filters = aiUsageFiltersFromParams(new URLSearchParams("start=2026-08-01&end=2026-08-10&feature=analysis_chat"), defaults);
  assert.deepEqual(filters, { start: "2026-08-01", end: "2026-08-10", feature: "analysis_chat" });
  assert.equal(aiUsageFilterParams(filters).toString(), "start=2026-08-01&end=2026-08-10&feature=analysis_chat");
  assert.equal(aiUsageFilterSummary(filters), "2026-08-01 to 2026-08-10 · Analysis chat");
  assert.equal(compactAIUsageFilterSummary(filters), "Aug 1–10 · Analysis chat");
  assert.equal(compactAIUsageFilterSummary({ start: "2026-07-30", end: "2026-08-02", feature: "" }), "Jul 30–Aug 2 · All features");
  assert.equal(compactAIUsageFilterSummary({ start: "2025-12-31", end: "2026-01-01", feature: "failure_analysis" }), "Dec 31, 2025–Jan 1, 2026 · Failure analysis");
  assert.equal(compactAIUsageFilterSummary({ start: "not-a-date", end: "2026-08-10", feature: "" }), "not-a-date–2026-08-10 · All features");
  assert.equal(aiUsageFiltersAreCustom(new URLSearchParams()), false);
  assert.equal(aiUsageFiltersAreCustom(new URLSearchParams("feature=analysis_chat")), true);
  assert.equal(coverageStateLabel("model_gateway_excluded"), "Model gateway excluded operations");
  assert.equal(featureTokenPercentage(25, 100), 25);
  assert.equal(featureTokenPercentage(0, 0), 0);
});

test("usage estimates preserve exact nanos and absent versus present zero", () => {
  assert.equal(formatRecordedUsageEstimate({ status: "available", nanos: "9007199254740993", currency: "USD" }), "USD 9,007,199.25");
  assert.equal(formatRecordedUsageEstimate({ status: "mixed_currency", nanos: "0" }), "Mixed currencies");
  assert.equal(formatRecordedUsageEstimate({ status: "unavailable", nanos: "0", currency: "USD" }), "Not priced");
  assert.equal(formatCurrentRateReprice({ status: "available", nanos: undefined, currency: "USD" }), "Unavailable");
  assert.equal(formatCurrentRateReprice({ status: "available", nanos: "0", currency: "USD" }), "USD 0.00");
  assert.equal(formatCurrentRateReprice({ status: "unavailable", nanos: "0", currency: "USD" }), "Unavailable");
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

test("daily cost chart maps the rendered view box inside letterboxed containers", () => {
  const wideLayout = chartViewBoxLayout(1600, 270, 960, 270);
  assert.deepEqual(wideLayout, { scale: 1, offsetX: 320, offsetY: 0 });
  assert.deepEqual(chartViewBoxPoint(560, 135, wideLayout), { x: 240, y: 135 });
  assert.equal(chartViewportX(240, wideLayout), 560);
  assert.deepEqual(chartViewBoxLayout(960, 540, 960, 270), { scale: 1, offsetX: 0, offsetY: 135 });
});

test("daily cost chart descriptions match visible series", () => {
  assert.match(chartSeriesDescription(true, false), /^The solid line shows recorded estimates\./);
  assert.doesNotMatch(chartSeriesDescription(true, false), /current-rate/);
  assert.match(chartSeriesDescription(false, true), /^The dashed line shows current-rate estimates\./);
  assert.doesNotMatch(chartSeriesDescription(false, true), /recorded estimates/);
  assert.match(chartSeriesDescription(true, true), /solid line.*dashed line/);
  assert.match(chartSeriesDescription(true, true), /ledger below/);
  assert.doesNotMatch(chartSeriesDescription(true, true), /table below/);
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
  assert.equal(pricedRequestCoverageNote(0, 0, "unavailable"), "Pricing coverage is unavailable");
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


test("daily cost chart opens on the newest mobile dates and avoids duplicate lines", () => {
  const source = readFileSync(resolve(process.cwd(), "src/components/AIUsageDaily.tsx"), "utf8");

  // The chart snaps to the newest dates when it starts overflowing, so the hint
  // below never claims they are in view while the oldest ones are.
  assert.match(source, /if \(overflowing && !wasScrollable\)[\s\S]{0,160}?scrollLeft = scroller\.scrollWidth - scroller\.clientWidth/);
  // Without this the chart would re-snap on every resize tick and fight a
  // reader who had scrolled back through earlier dates.
  assert.match(source, /wasScrollable = overflowing/);
  assert.match(source, /Newest dates are in view\. Scroll left for earlier dates\./);
  assert.match(source, /const matchingSeries =/);
  assert.match(source, /Current-rate estimate matches the recorded estimate\./);
  assert.match(source, /Peak day:/);
});
