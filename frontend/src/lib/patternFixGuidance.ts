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

// causalGroupFixTarget returns the failure a cause is actually built from, when
// that failure can start a Fix investigation. Returns null when the cause offers
// no reachable target.
export function causalGroupFixTarget(
  group: PatternCausalGroup,
  runs: BuildResult[],
): CausalGroupFixTarget | null {
  const affectedBuilds = new Set(group.builds);
  for (const run of runs) {
    if (!affectedBuilds.has(run.build_id)) continue;
    const occurrences = run.test_cases;
    const representative = representativeAnalyzedFailure(occurrences);
    if (!representative || !fixInvestigationEligible(representative)) continue;
    // The test detail page resolves a name to the first matching case in the
    // build, so a shadowed representative is unreachable. Only offer a target
    // the destination will actually open.
    if (occurrences.find((occurrence) => occurrence.name === representative.name) !== representative) continue;
    return { buildID: run.build_id, testName: representative.name };
  }
  return null;
}

const severityRank: Record<string, number> = {
  critical: 5,
  high: 4,
  medium: 3,
  low: 2,
  "transient-ignore": 1,
};

function rank(severity: string | undefined): number {
  return severityRank[severity?.trim().toLowerCase() ?? ""] ?? 0;
}

// representativeAnalyzedFailure mirrors the backend selection in
// patterns.RepresentativeAnalyzedFailure. A causal group's root cause is derived
// from this exact failure per build, so routing anywhere else would send the
// user to a failure the cause does not describe.
function representativeAnalyzedFailure(testCases: TestCase[]): TestCase | null {
  let representative: TestCase | null = null;
  for (const testCase of testCases) {
    if (testCase.status !== "failed" || !testCase.ai_analysis) continue;
    if (!representative || rank(testCase.ai_analysis.severity) > rank(representative.ai_analysis?.severity)) {
      representative = testCase;
    }
  }
  return representative;
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
