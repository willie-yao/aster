import type { TestCase } from "../types/dashboard";
import type { FetchStatusResponse } from "../types/fetchStatus";

export function isBuildFailure(testCase: TestCase): boolean {
  return testCase.source === "build";
}

export function junitTestCases(testCases: TestCase[] = []): TestCase[] {
  return testCases.filter((testCase) => !isBuildFailure(testCase));
}

export function buildFailure(testCases: TestCase[] = []): TestCase | undefined {
  return testCases.find(isBuildFailure);
}

export type BuildAnalysisState = "queued" | "running" | "unavailable" | "stale" | "succeeded";

export function buildAnalysisState(failure: TestCase, response: FetchStatusResponse | null): BuildAnalysisState {
  if (failure.ai_analysis) return "succeeded";
  if (response?.state === "stale") return "stale";
  const status = response?.status;
  if (response?.state === "active" && status) {
    const builds = status.analyses.build_subjects;
    if (status.phase === "analysis" && (builds?.running ?? 0) > 0) return "running";
    if (["artifacts", "aggregation", "analysis-planning", "analysis"].includes(status.phase) && (builds?.queued ?? 0) > 0) return "queued";
  }
  return "unavailable";
}
