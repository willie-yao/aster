import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  currentJobStatus,
  executedResultTests,
  filterResultTests,
  hasInlineTestEvidence,
  normalizeResultLedgerFilter,
  recentJobPassRate,
  withJobDetailParam,
} from "../src/lib/jobDetail.js";
import type { BuildResult, TestCase } from "../src/types/dashboard.js";

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

function build(buildID: string, passed: boolean): BuildResult {
  return {
    build_id: buildID,
    job_name: "job",
    started: "2026-08-10T00:00:00Z",
    finished: "2026-08-10T01:00:00Z",
    passed,
    result: passed ? "SUCCESS" : "FAILURE",
    duration_seconds: 3600,
    commit: "0123456789abcdef0123456789abcdef01234567",
    prow_url: "https://prow.example/build",
    web_url: "https://storage.example/build",
    build_log_url: "https://storage.example/build-log.txt",
    test_cases: [],
    tests_total: 0,
    tests_passed: 0,
    tests_failed: 0,
    tests_skipped: 0,
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

test("inline evidence includes analyzed failures without a failure message", () => {
  assert.equal(
    hasInlineTestEvidence(
      testCase({
        status: "failed",
        failure_body: "stack trace",
      }),
    ),
    true,
  );
  assert.equal(
    hasInlineTestEvidence(
      testCase({
        status: "failed",
        ai_analysis: {
          generated_at: "2026-08-07T00:00:00Z",
          model: "test",
          root_cause: "cause",
          severity: "High",
          suggested_fix: "fix",
        },
      }),
    ),
    true,
  );
  assert.equal(hasInlineTestEvidence(testCase({ status: "failed" })), false);
  assert.equal(
    hasInlineTestEvidence(
      testCase({ status: "passed", failure_body: "historical output" }),
    ),
    false,
  );
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

test("job detail separates current observation from rolling reliability", () => {
  const runs = [
    ...Array.from({ length: 5 }, (_, index) => build(`pass-${5 - index}`, true)),
    ...Array.from({ length: 5 }, (_, index) => build(`failure-${5 - index}`, false)),
  ];
  assert.equal(currentJobStatus(undefined, runs), "PASSING");
  assert.equal(recentJobPassRate(runs), 0.5);
  assert.equal(currentJobStatus("FAILING", runs), "FAILING");
});

test("job detail uses the approved shared detail composition", () => {
  const page = source("src/pages/JobDetailPage.tsx");
  const pattern = source("src/components/PatternBanner.tsx");
  const briefing = source("src/components/AnalysisBriefing.tsx");
  const buildFailure = source("src/components/BuildFailurePanel.tsx");
  const identity = source("src/components/TechnicalIdentity.tsx");
  const testTable = source("src/components/TestCaseTable.tsx");
  const analysis = source("src/components/AiAnalysisPanel.tsx");
  const ledger = source("src/components/ResultLedger.tsx");

  assert.match(page, /shortJobName\([\s\S]*manifest\.short_name_prefix/);
  assert.match(page, /<TechnicalIdentity[\s\S]*Canonical job ID[\s\S]*Copy canonical job ID/);
  assert.match(page, /<MetricStrip items=\{metricItems\} label="Job metrics"/);
  assert.match(page, /label: "Current"/);
  assert.match(page, /label: "Last 10 runs"/);
  assert.match(page, /label: "Recovery streak"/);
  assert.match(page, /label: "Median duration"/);
  assert.match(page, /label: "95th percentile"/);
  assert.match(page, /runHistory=\{runHistory\}[\s\S]*runtimeTrend=\{[\s\S]*<RuntimeTrend summary=\{runtimeSummary\} subject=\{displayName\}/);
  assert.match(page, /<RunMetadata[\s\S]*View in Prow[\s\S]*Build log/);
  assert.match(page, /<TestResultsGrid runs=\{runs\}/);
  assert.match(page, /<ResultLedger[\s\S]*executedCount[\s\S]*skippedCount/);
  assert.match(page, /updateSearchParam\("results", filter\)/);
  assert.match(page, /updateSearchParam\("test", query \|\| null, \{ replace: true \}\)/);

  assert.match(pattern, /<AnalysisBriefing/);
  assert.match(pattern, /icon=\{<AutoAwesome/);
  assert.match(pattern, /mobileNotice=\{mobileNotice\}/);
  assert.match(pattern, /Last successful refresh ·/);
  assert.match(pattern, /label="Dismissed"/);
  assert.match(pattern, /Dismissed by/);
  assert.match(pattern, /<AnalysisChat[\s\S]*appearance="detail"/);
  assert.match(pattern, /<FailureActions[\s\S]*appearance="detail"/);
  assert.match(pattern, /label="Root cause"/);
  assert.match(pattern, /label="Suggested remediation"/);
  assert.match(pattern, /label="Source grounding"/);
  assert.match(pattern, /label="Affected builds"/);
  assert.match(pattern, /label="Related files"/);
  assert.match(pattern, /<CausalGroupRemediation/);
  assert.match(pattern, /investigation=\{group\.content_hash \? remediationByHash\.get\(group\.content_hash\) : undefined\}/);
  assert.match(pattern, /patternID=\{pattern\.id\}/);
  assert.match(pattern, /patternHash=\{pattern\.content_hash\}/);
  assert.match(pattern, /Remediation present, verifying the fix/);
  assert.match(pattern, /Watching recovery/);
  assert.match(pattern, /Observed passing runs:/);
  assert.match(pattern, /Verified passing runs:/);
  assert.match(pattern, /Verified remediation source:/);
  assert.match(pattern, /lifecycleActive/);

  assert.match(briefing, /mobileNotice[\s\S]*\{mobileNotice &&/);
  assert.match(buildFailure, /<AnalysisBriefing/);
  assert.match(buildFailure, /<AiAnalysisPanel[\s\S]*appearance="detail"/);
  assert.match(buildFailure, /<FailureActions[\s\S]*appearance="detail"/);
  assert.match(buildFailure, /Open build failure details/);
  assert.doesNotMatch(buildFailure, /detailAppearance/);
  assert.match(identity, /display: \{ xs: "none", md: "flex" \}/);
  assert.match(identity, /desktopInline \? \{ xs: "block", md: "none" \}/);
  assert.match(testTable, /Evidence/);
  assert.match(testTable, /Analysis →/);
  assert.match(testTable, /Show inline evidence/);
  assert.match(testTable, /<AiAnalysisPanel[\s\S]*appearance="detail"/);
  assert.doesNotMatch(testTable, /<Panel/);
  assert.match(analysis, /appearance\?: "default" \| "detail"/);
  assert.match(analysis, /label="Root cause"/);
  assert.match(analysis, /label="Suggested remediation"/);
  assert.match(analysis, />\s*Related files\s*</);
  assert.doesNotMatch(analysis, /Suggested Fix/);

  assert.match(ledger, /failed: "Failed"[\s\S]*passed: "Passed"[\s\S]*all: "All statuses"/);
  assert.doesNotMatch(ledger, /Skipped"/);
  assert.doesNotMatch(page, /virtual/i);
});
