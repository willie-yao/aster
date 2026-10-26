import type { AnalysisCauseLocation, BuildResult, PatternAnalysis, PatternCausalGroup, TestCase } from "../types/dashboard";
import { executedResultTests } from "./jobDetail.js";
import { buildFailure } from "./buildFailures.js";

export const failedTestGridID = "cross-run-test-grid";

export interface CausalGroupFixTarget {
  buildID: string;
  testName: string;
  source?: "build";
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

// causalGroupEvidencePresent reports whether any build this cause names is still
// in the job window. It separates the two reasons a cause offers no fix target:
// its builds have left the window, or they are here and no failure in them
// qualifies. Retained runs carry no test cases, so they are not evidence.
export function causalGroupEvidencePresent(
  group: PatternCausalGroup,
  runs: BuildResult[],
): boolean {
  const affectedBuilds = new Set(group.builds);
  return runs.some((run) => affectedBuilds.has(run.build_id));
}

// causalGroupFixTarget returns the failure a cause is actually built from, when
// that failure can start a fix proposal. Returns null when the cause offers
// no reachable target.
export function causalGroupFixTarget(
  group: PatternCausalGroup,
  runs: BuildResult[],
): CausalGroupFixTarget | null {
  const affectedBuilds = new Set(group.builds);
  // The briefing shows the suggested fix from one specific build, so open that
  // build's analysis when it can start a Fix investigation. Otherwise any
  // eligible member still gives the user a way in.
  const preferred = causalGroupNextStep(group, runs)?.buildID;
  const ordered = preferred
    ? [...runs].sort((a, b) => Number(b.build_id === preferred) - Number(a.build_id === preferred))
    : runs;
  for (const run of ordered) {
    if (!affectedBuilds.has(run.build_id) || run.passed) continue;
    const occurrences = run.test_cases;
    const representative = representativeAnalyzedFailure(occurrences);
    if (!representative || !fixInvestigationEligible(representative)) continue;
    const destinationFailure = representative.source === "build"
      ? buildFailure(occurrences)
      : occurrences.find((occurrence) => occurrence.name === representative.name);
    if (destinationFailure !== representative) continue;
    return {
      buildID: run.build_id,
      testName: representative.name,
      ...(representative.source === "build" ? { source: "build" as const } : {}),
    };
  }
  return null;
}

// compareBuildIDs orders Prow build IDs, which are decimal and grow with time.
function compareBuildIDs(a: string, b: string): number {
  return a.length - b.length || (a < b ? -1 : a > b ? 1 : 0);
}

function newestBuildID(builds: string[]): string {
  return builds.reduce((newest, id) => (compareBuildIDs(id, newest) > 0 ? id : newest), "");
}

// orderCausalGroups puts the cause that reaches the newest build first, so the
// cause still failing leads and older ones follow. Ties keep the larger group,
// then the engine's order.
export function orderCausalGroups(groups: PatternCausalGroup[]): PatternCausalGroup[] {
  return groups
    .map((group, index) => ({ group, index, newest: newestBuildID(group.builds) }))
    .sort((a, b) =>
      compareBuildIDs(b.newest, a.newest) ||
      b.group.builds.length - a.group.builds.length ||
      a.index - b.index)
    .map(({ group }) => group);
}

// causalGroupIsEarlier reports whether a cause stopped failing before the lead
// cause's newest build.
export function causalGroupIsEarlier(group: PatternCausalGroup, lead: PatternCausalGroup): boolean {
  return compareBuildIDs(newestBuildID(group.builds), newestBuildID(lead.builds)) < 0;
}

export interface CausalGroupNextStepSource {
  suggestedFix: string;
  buildID: string;
}

// causalGroupNextStep returns the remediation from the newest affected build
// that still has an analyzed failure, which is the one the Test detail page
// shows. It falls back to the engine's carried remediation when no affected run
// is in the window.
export function causalGroupNextStep(
  group: PatternCausalGroup,
  runs: BuildResult[],
): CausalGroupNextStepSource | null {
  const affected = new Set(group.builds);
  const newestFirst = runs
    .filter((run) => affected.has(run.build_id) && !run.passed)
    .sort((a, b) => compareBuildIDs(b.build_id, a.build_id));
  for (const run of newestFirst) {
    const suggestedFix = representativeAnalyzedFailure(run.test_cases)?.ai_analysis?.suggested_fix?.trim();
    if (suggestedFix) return { suggestedFix, buildID: run.build_id };
  }
  const carried = group.remediation?.suggested_fix?.trim();
  return carried && group.remediation
    ? { suggestedFix: carried, buildID: group.remediation.build_id }
    : null;
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
    const analysis = testCase.ai_analysis;
    if (testCase.status !== "failed" || !analysisHasUsableDiagnosis(analysis)) continue;
    if (!representative || rank(analysis.severity) > rank(representative.ai_analysis?.severity)) {
      representative = testCase;
    }
  }
  return representative;
}

function analysisHasUsableDiagnosis(
  analysis: TestCase["ai_analysis"],
): analysis is NonNullable<TestCase["ai_analysis"]> {
  if (!analysis) return false;
  return analysis.disposition === "citations_verified" || analysis.disposition === "preliminary";
}

// fixInvestigationEligible keeps the exact-failure structural boundary. Analysis
// quality and source hints are context for the coding agent, not admission
// requirements for a maintainer-requested investigation.
function fixInvestigationEligible(testCase: TestCase): boolean {
  return testCase.status === "failed" &&
    (testCase.source === "build" ? !testCase.junit_file : Boolean(testCase.junit_file));
}

// externalCause returns a cause location only when it names a dependency. A
// cause in the project's own repository keeps the ordinary Fix route, so
// ownership must never turn an own-repo failure into an upstream dead end.
export function externalCause(
  location: AnalysisCauseLocation | undefined,
): AnalysisCauseLocation | null {
  return location?.external && location.repository ? location : null;
}

// patternExternalCause returns the dependency a whole pattern points at, which
// exists only when every cause it found agrees. Mixed ownership keeps the
// generic guidance because no single upstream repository explains the pattern.
// Repository comparison is case-insensitive to match the backend's merge.
export function patternExternalCause(
  pattern: PatternAnalysis,
): AnalysisCauseLocation | null {
  const groups = pattern.causal_groups ?? [];
  if (groups.length === 0) return null;
  const first = externalCause(groups[0].cause_location);
  if (!first) return null;
  const owner = first.repository.toLowerCase();
  for (const group of groups.slice(1)) {
    const location = externalCause(group.cause_location);
    if (!location || location.repository.toLowerCase() !== owner) return null;
  }
  return first;
}
