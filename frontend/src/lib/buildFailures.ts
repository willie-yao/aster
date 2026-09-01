import type { AIAnalysis, TestCase } from "../types/dashboard";
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

export type BuildAnalysisState = "pending" | "unavailable" | "stale" | "succeeded";

export function buildAnalysisState(failure: TestCase, response: FetchStatusResponse | null): BuildAnalysisState {
  if (failure.ai_analysis) return "succeeded";
  if (response?.state === "stale") return "stale";
  const status = response?.status;
  if (response?.state === "active" && status) {
    const builds = status.analyses.build_subjects;
    if (["artifacts", "aggregation", "analysis-planning", "analysis"].includes(status.phase) && ((builds?.queued ?? 0) > 0 || (builds?.running ?? 0) > 0)) return "pending";
  }
  return "unavailable";
}

export function buildFailureActionID(jobID: string, buildID: string): string {
  const encode = (value: string) => {
    const bytes = new TextEncoder().encode(value);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  return `build::${encode(jobID)}::${encode(buildID)}`;
}

export function buildActionsReady(analysis: AIAnalysis | undefined, currentCritiqueVersion: number | undefined): boolean {
  return Boolean(
    analysis?.mode === "agentic" &&
    analysis.disposition === "citations_verified" &&
    analysis.critique_passed &&
    currentCritiqueVersion != null &&
    (analysis.critique_version ?? 0) >= currentCritiqueVersion &&
    analysis.generated_at &&
    analysis.root_cause.trim() &&
    analysis.suggested_fix.trim(),
  );
}
