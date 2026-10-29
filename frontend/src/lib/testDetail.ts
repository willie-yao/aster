import type { BuildResult, PatternAnalysis, TestCase } from "../types/dashboard";
import type { FileToUrlContext } from "./utils";

// firstSentence returns the leading sentence of an analysis for a synopsis.
export function firstSentence(value: string): string {
  const match = value.trim().match(/^.*?[.!?](?:\s|$)/u);
  return match?.[0].trim() || value.trim();
}

// testRunFileContext resolves artifact and source links for one test case in
// one run.
export function testRunFileContext(
  run: BuildResult | null | undefined,
  testCase: TestCase,
): FileToUrlContext {
  return {
    buildLogUrl: run?.build_log_url,
    webUrl: run?.web_url,
    fileLinks: testCase.ai_analysis?.file_links,
  };
}

// fixPatternsForRun selects the published patterns a fix from this run can
// cite.
export function fixPatternsForRun(
  patterns: PatternAnalysis[] | undefined,
  buildID: string,
): PatternAnalysis[] {
  return (patterns ?? []).filter(
    (pattern) =>
      !pattern.recurrence_classification &&
      Boolean(pattern.id) &&
      Boolean(pattern.content_hash) &&
      Boolean(pattern.shared_builds?.includes(buildID)),
  );
}

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
  persistentAfter: number,
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
  // Mirrors the backend's classifyOutcomes: below the persistent threshold, a
  // history with failures but no passes is one-off, not unclassified.
  const classification =
    consecutiveFailures >= persistentAfter
      ? `Persistent (${consecutiveFailures}×)`
      : failed > 1 && passed > 0
        ? "Flaky"
        : failed > 0
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
