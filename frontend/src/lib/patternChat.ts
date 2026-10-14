import type { AnalysisChatSession } from "../types/analysisChat";
import type { BuildResult, PatternAnalysis, PatternCausalGroup } from "../types/dashboard";

export type PatternChatAvailability = "ready" | "stale" | "unavailable";

export function causeSuggestedQuestions(
  baselineBuildID?: string,
  session?: Pick<AnalysisChatSession, "baseline_build_id"> | null,
): string[] {
  const baseline = session ? session.baseline_build_id : baselineBuildID;
  return [
    "What evidence supports this cause across its builds?",
    ...(baseline ? ["What changed since the last passing run?"] : []),
    "Has this cause been resolved in the latest completed run?",
    "What concrete change follows from this cause?",
    "Is there a section of code that caused this failure?",
  ];
}

// Mirrors analysischat.selectCauseBaselineRun, including its newest-first
// ordering. Truncated dashboard JUnit does not disqualify artifact access.
export function causeBaselineBuildID(group: PatternCausalGroup, runs: BuildResult[]): string | undefined {
  const members = new Set(group.builds.map((buildID) => buildID.trim()).filter(Boolean));
  const ordered = [...runs].sort((left, right) => {
    const started = Date.parse(right.started) - Date.parse(left.started);
    if (started !== 0) return started;
    if (/^\d+$/.test(left.build_id) && /^\d+$/.test(right.build_id)) {
      const leftID = BigInt(left.build_id);
      const rightID = BigInt(right.build_id);
      if (leftID !== rightID) return leftID > rightID ? -1 : 1;
    }
    return left.build_id < right.build_id ? 1 : left.build_id > right.build_id ? -1 : 0;
  });
  const oldestMember = ordered.findLastIndex((run) => members.has(run.build_id));
  if (oldestMember < 0) return undefined;
  return ordered.slice(oldestMember + 1).find((run) =>
    run.passed && run.result === "SUCCESS" && run.tests_failed === 0 && Boolean(run.junit_urls?.length))
    ?.build_id;
}

export function patternChatEvidenceBuildIDs(pattern: PatternAnalysis): string[] {
  const repeatedGroups = pattern.causal_groups?.filter((group) => group.builds.length >= 2) ?? [];
  const candidates = repeatedGroups.length > 0
    ? repeatedGroups.flatMap((group) => group.builds)
    : pattern.shared_builds ?? [];
  return [...new Set(candidates.map((buildID) => buildID.trim()).filter(Boolean))];
}

export function patternChatHasEvidenceBuild(
  pattern: PatternAnalysis,
  currentBuildIDs: Iterable<string>,
  requireComplete: boolean,
): boolean {
  const expected = patternChatEvidenceBuildIDs(pattern);
  if (expected.length === 0) return false;
  const current = new Set(currentBuildIDs);
  return requireComplete
    ? expected.every((buildID) => current.has(buildID))
    : expected.some((buildID) => current.has(buildID));
}

export function patternChatAvailability(
  pattern: PatternAnalysis,
  jobID: string | undefined,
  hasEvidenceBuild: boolean,
  chatEnabled: boolean,
): PatternChatAvailability {
  if (!chatEnabled || !pattern.id || !jobID || !hasEvidenceBuild) return "unavailable";
  return pattern.content_hash ? "ready" : "stale";
}
