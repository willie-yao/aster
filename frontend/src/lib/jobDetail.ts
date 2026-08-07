import type { TestCase } from "../types/dashboard";
import { junitTestCases } from "./buildFailures.js";

export type ResultLedgerFilter = "failed" | "passed" | "all";

const setupPatterns =
  /synchronizedbeforesuite|synchronizedaftersuite|beforesuite|aftersuite/iu;

export function normalizeResultLedgerFilter(
  value: string | null,
): ResultLedgerFilter {
  return value === "passed" || value === "all" ? value : "failed";
}

export function executedResultTests(testCases: TestCase[]): TestCase[] {
  return junitTestCases(testCases).filter(
    (testCase) =>
      testCase.status !== "skipped" &&
      (testCase.status === "failed" || !setupPatterns.test(testCase.name)),
  );
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
