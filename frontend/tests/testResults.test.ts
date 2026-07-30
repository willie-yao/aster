import assert from "node:assert/strict";
import { test } from "node:test";

import { emptyTestResultsPresentation } from "../src/lib/testResults.js";
import type { BuildResult } from "../src/types/dashboard.js";

function run(overrides: Partial<BuildResult> = {}): BuildResult {
  return {
    build_id: "1",
    job_name: "job",
    started: "2026-07-29T02:49:10Z",
    finished: "2026-07-29T02:53:18Z",
    passed: false,
    result: "FAILURE",
    duration_seconds: 248,
    commit: "deadbeef",
    prow_url: "https://prow.example/run",
    web_url: "https://artifacts.example/run/",
    build_log_url: "https://artifacts.example/run/build-log.txt",
    junit_complete: true,
    test_cases: [],
    tests_total: 0,
    tests_passed: 0,
    tests_failed: 0,
    tests_skipped: 0,
    ...overrides,
  };
}

test("failed runs without JUnit explain that no results were reported", () => {
  assert.deepEqual(emptyTestResultsPresentation(run()), {
    kind: "failed",
    title: "No test results were reported",
    detail: "This run failed without uploading JUnit test results. It may have stopped during setup or before the test suite could report results. Review the build log for the failure.",
    severity: "error",
  });
});

test("incomplete discovery does not claim that tests never started", () => {
  assert.equal(
    emptyTestResultsPresentation(run({ junit_complete: false }))?.title,
    "Test results unavailable",
  );
});

test("truncated discovery remains incomplete when some JUnit URLs were found", () => {
  assert.deepEqual(
    emptyTestResultsPresentation(run({
      junit_complete: false,
      junit_truncated: true,
      junit_urls: ["https://artifacts.example/run/junit.xml"],
    })),
    {
      kind: "unavailable",
      title: "Test results incomplete",
      detail: "JUnit discovery reached the artifact limit, so some test cases may be missing. Review the build artifacts and build log for the complete result.",
      severity: "warning",
    },
  );
});

test("uploaded JUnit without parsed cases reports unreadable results", () => {
  assert.equal(
    emptyTestResultsPresentation(run({ junit_urls: ["https://artifacts.example/run/junit.xml"] }))?.title,
    "No readable test cases",
  );
});

test("completed passing runs without JUnit use a neutral empty state", () => {
  assert.equal(
    emptyTestResultsPresentation(run({ passed: true, result: "SUCCESS" }))?.title,
    "No JUnit test results",
  );
});

test("pending and populated runs retain their normal presentation", () => {
  assert.equal(emptyTestResultsPresentation(run({ result: "PENDING" }))?.title, "Build still running");
  assert.equal(emptyTestResultsPresentation(run({
    test_cases: [{ name: "test", status: "failed", duration_seconds: 1 }],
  })), null);
});
