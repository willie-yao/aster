import assert from "node:assert/strict"; import { test } from "node:test";
import { formatCost, formatCoverage, formatTokens, pricedRequestCoverageNote, totalTokens, usageQuery } from "../src/lib/aiUsage.js";
import type { AIUsageReport, AIUsageTotals } from "../src/types/usage.js";
test("AI usage helpers format values and filters",()=>{ assert.equal(formatTokens(1200),"1,200"); assert.match(formatCost("1250000","USD"),/^\$0\.00125/); assert.equal(totalTokens({input_tokens:2,output_tokens:3} as never),5); assert.equal(formatCoverage(7,10),"7 of 10 (70%)"); assert.equal(usageQuery("2026-08-01","2026-08-03","analysis_chat"),"start=2026-08-01&end=2026-08-03&feature=analysis_chat"); });


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
  assert.equal(
    pricedRequestCoverageNote(
      legacyTotals.priced_reported_requests ?? legacyCoverage.priced_reported_requests,
      legacyTotals.reported_requests,
      "partial",
    ),
    "Priced-request coverage is unavailable for legacy records",
  );
  assert.equal(
    pricedRequestCoverageNote(4, 7, "partial"),
    "4 of 7 (57%) reported requests priced",
  );
});
