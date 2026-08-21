import type { BuildResult, JobCurrentStatus, TestCase } from "../types/dashboard";
import { junitTestCases } from "./buildFailures.js";

export type ResultLedgerFilter = "failed" | "passed" | "all";

const setupPatterns =
  /synchronizedbeforesuite|synchronizedaftersuite|beforesuite|aftersuite/iu;

export function normalizeResultLedgerFilter(
  value: string | null,
  fallback: ResultLedgerFilter = "failed",
): ResultLedgerFilter {
  return value === "failed" || value === "passed" || value === "all" ? value : fallback;
}

export interface ResultTestSummary {
  executed: TestCase[];
  visible: TestCase[];
  hiddenSuccessfulSetupTeardown: number;
}

export function summarizeResultTests(testCases: TestCase[]): ResultTestSummary {
  const executed = junitTestCases(testCases).filter(
    (testCase) => testCase.status !== "skipped",
  );
  const visible = executed.filter(
    (testCase) =>
      testCase.status === "failed" || !setupPatterns.test(testCase.name),
  );
  return {
    executed,
    visible,
    hiddenSuccessfulSetupTeardown: executed.length - visible.length,
  };
}

export function executedResultTests(testCases: TestCase[]): TestCase[] {
  return summarizeResultTests(testCases).visible;
}

export function filterResultTests(
  testCases: TestCase[],
  filter: ResultLedgerFilter,
  query: string,
): TestCase[] {
  const normalizedQuery = query.trim().toLocaleLowerCase();
  return testCases.filter((testCase) => {
    if (filter !== "all" && testCase.status !== filter) return false;
    if (!normalizedQuery) return true;
    return [
      testCase.name,
      testCase.failure_message,
      testCase.failure_body,
      testCase.failure_location,
    ]
      .filter(Boolean)
      .join(" ")
      .toLocaleLowerCase()
      .includes(normalizedQuery);
  });
}

const resultStatusOrder: Record<TestCase["status"], number> = {
  failed: 0,
  passed: 1,
  skipped: 2,
};

export function sortResultTests(testCases: TestCase[]): TestCase[] {
  return [...testCases].sort(
    (a, b) => resultStatusOrder[a.status] - resultStatusOrder[b.status],
  );
}

export function hasInlineTestEvidence(testCase: TestCase): boolean {
  return Boolean(
    testCase.status === "failed" &&
      (testCase.failure_message ||
        testCase.failure_body ||
        testCase.cluster_artifacts ||
        testCase.ai_analysis),
  );
}

export function withJobDetailParam(
  current: URLSearchParams,
  name: string,
  value: string | null,
): URLSearchParams {
  const next = new URLSearchParams(current);
  if (value) next.set(name, value);
  else next.delete(name);
  return next;
}

export function currentJobStatus(
  published: JobCurrentStatus | undefined,
  runs: BuildResult[],
): JobCurrentStatus {
  if (published) return published;
  const latest = runs[0];
  if (!latest) return "UNKNOWN";
  if (latest.result === "PENDING") return "RUNNING";
  return latest.passed ? "PASSING" : "FAILING";
}

export function recentJobPassRate(runs: BuildResult[], limit = 10): number | null {
  if (runs.length === 0) return null;
  const recent = runs.slice(0, limit);
  return recent.filter((run) => run.passed).length / recent.length;
}
