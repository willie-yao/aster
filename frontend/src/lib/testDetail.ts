import type { TestCase } from "../types/dashboard";

export interface TestHistoryOccurrence {
  testCase: Pick<TestCase, "status"> | null;
}

export interface TestHistorySummary {
  failed: number;
  passed: number;
  skipped: number;
  absent: number;
  executed: number;
  failureRate: number | null;
  consecutiveFailures: number;
  classification: string | null;
}

export function summarizeTestHistory(
  occurrences: TestHistoryOccurrence[],
): TestHistorySummary {
  let failed = 0;
  let passed = 0;
  let skipped = 0;
  let absent = 0;

  for (const occurrence of occurrences) {
    switch (occurrence.testCase?.status) {
      case "failed":
        failed += 1;
        break;
      case "passed":
        passed += 1;
        break;
      case "skipped":
        skipped += 1;
        break;
      default:
        absent += 1;
    }
  }

  let consecutiveFailures = 0;
  for (let index = occurrences.length - 1; index >= 0; index -= 1) {
    const status = occurrences[index].testCase?.status;
    if (!status || status === "skipped") continue;
    if (status === "failed") consecutiveFailures += 1;
    else break;
  }

  const executed = failed + passed;
  const failureRate = executed > 0 ? failed / executed : null;
  const classification =
    consecutiveFailures >= 3
      ? `Persistent (${consecutiveFailures}×)`
      : failed > 1 && passed > 0
        ? "Flaky"
        : failed === 1
          ? "One-off failure"
          : null;

  return {
    failed,
    passed,
    skipped,
    absent,
    executed,
    failureRate,
    consecutiveFailures,
    classification,
  };
}
