import type { BuildResult, PatternAnalysis, PatternCausalGroup, TestCase } from "../types/dashboard";
import { executedResultTests } from "./jobDetail.js";

export const failedTestGridID = "cross-run-test-grid";

export interface CausalGroupFixTarget {
  buildID: string;
  testName: string;
}

export function patternFixGuidanceBuildID(
  pattern: PatternAnalysis,
  runs: BuildResult[],
): string | null {
  if (!pattern.recurrence_classification || !pattern.causal_groups?.length) return null;

  const affectedBuilds = new Set(pattern.causal_groups.flatMap((group) => group.builds));
  return runs.find((run) =>
    affectedBuilds.has(run.build_id) &&
    executedResultTests(run.test_cases).some((testCase) => testCase.status === "failed"))
    ?.build_id ?? null;
}

// causalGroupFixTarget returns the first failed test in the group's builds that
// can actually start a Fix investigation, or null when the cause offers none.
export function causalGroupFixTarget(
  group: PatternCausalGroup,
  runs: BuildResult[],
): CausalGroupFixTarget | null {
  const affectedBuilds = new Set(group.builds);
  for (const run of runs) {
    if (!affectedBuilds.has(run.build_id)) continue;
    const occurrences = run.test_cases;
    // The test detail page resolves a name to the first matching case in the
    // build, so a later eligible occurrence of a repeated name is unreachable.
    // Only offer a target the destination will actually open.
    const testCase = executedResultTests(occurrences).find(
      (candidate) =>
        fixInvestigationEligible(candidate) &&
        occurrences.find((occurrence) => occurrence.name === candidate.name) === candidate,
    );
    if (testCase) return { buildID: run.build_id, testName: testCase.name };
  }
  return null;
}

// fixInvestigationEligible applies the part of the server Fix gate that is
// decidable from published data. An analysis with no file links has no verified
// source path, which is conclusive before a chat session resolves the source
// repository.
function fixInvestigationEligible(testCase: TestCase): boolean {
  return testCase.status === "failed" &&
    testCase.source !== "build" &&
    Boolean(testCase.junit_file) &&
    Object.keys(testCase.ai_analysis?.file_links ?? {}).length > 0;
}
