import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  executedResultTests,
  filterResultTests,
  normalizeResultLedgerFilter,
  withJobDetailParam,
} from "../src/lib/jobDetail.js";
import type { TestCase } from "../src/types/dashboard.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

function testCase(overrides: Partial<TestCase>): TestCase {
  return {
    name: "executes workload",
    status: "passed",
    duration_seconds: 1,
    ...overrides,
  };
}

test("job result filters omit skipped and non-failing setup rows", () => {
  const executed = executedResultTests([
    testCase({ name: "passes", status: "passed" }),
    testCase({ name: "fails", status: "failed", failure_message: "boom" }),
    testCase({ name: "skips", status: "skipped" }),
    testCase({ name: "BeforeSuite", status: "passed" }),
    testCase({ name: "AfterSuite", status: "failed", failure_message: "setup failed" }),
  ]);

  assert.deepEqual(executed.map((item) => item.name), ["passes", "fails", "AfterSuite"]);
  assert.deepEqual(filterResultTests(executed, "failed", "").map((item) => item.name), ["fails", "AfterSuite"]);
  assert.deepEqual(filterResultTests(executed, "passed", "").map((item) => item.name), ["passes"]);
  assert.deepEqual(filterResultTests(executed, "all", "SETUP FAILED").map((item) => item.name), ["AfterSuite"]);
});

test("job result filter state uses bounded URL values", () => {
  assert.equal(normalizeResultLedgerFilter("failed"), "failed");
  assert.equal(normalizeResultLedgerFilter("passed"), "passed");
  assert.equal(normalizeResultLedgerFilter("all"), "all");
  assert.equal(normalizeResultLedgerFilter("skipped"), "failed");

  const current = new URLSearchParams("run=123&results=passed&test=pod&failure=pattern-1");
  const next = withJobDetailParam(current, "run", "456");
  assert.equal(next.toString(), "run=456&results=passed&test=pod&failure=pattern-1");
  const cleared = withJobDetailParam(next, "test", null);
  assert.equal(cleared.toString(), "run=456&results=passed&failure=pattern-1");
});

test("job detail uses the approved shared detail composition", () => {
  const page = source("src/pages/JobDetailPage.tsx");
  const pattern = source("src/components/PatternBanner.tsx");
  const ledger = source("src/components/ResultLedger.tsx");

  assert.match(page, /shortJobName\([\s\S]*manifest\.short_name_prefix/);
  assert.match(page, /<TechnicalIdentity[\s\S]*Canonical job ID[\s\S]*Copy canonical job ID/);
  assert.match(page, /<MetricStrip items=\{metricItems\} label="Job metrics"/);
  assert.match(page, /<RunMetadata[\s\S]*View in Prow[\s\S]*Build log/);
  assert.match(page, /<TestResultsGrid runs=\{runs\}/);
  assert.match(page, /<ResultLedger[\s\S]*executedCount[\s\S]*skippedCount/);
  assert.match(page, /updateSearchParam\("results", filter\)/);
  assert.match(page, /updateSearchParam\("test", query \|\| null, \{ replace: true \}\)/);

  assert.match(pattern, /<AnalysisBriefing/);
  assert.match(pattern, /<AnalysisChat[\s\S]*appearance="detail"/);
  assert.match(pattern, /<FailureActions[\s\S]*appearance="detail"/);
  assert.match(pattern, /label="Root cause"/);
  assert.match(pattern, /label="Suggested remediation"/);
  assert.match(pattern, /label="Source grounding"/);
  assert.match(pattern, /label="Affected builds"/);
  assert.match(pattern, /label="Related files"/);

  assert.match(ledger, /failed: "Failed"[\s\S]*passed: "Passed"[\s\S]*all: "All executed"/);
  assert.doesNotMatch(ledger, /Skipped"/);
  assert.doesNotMatch(page, /virtual/i);
});
