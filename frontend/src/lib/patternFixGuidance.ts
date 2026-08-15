import type { BuildResult, PatternAnalysis } from "../types/dashboard";
import { executedResultTests } from "./jobDetail.js";

export const failedTestGridID = "cross-run-test-grid";

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
