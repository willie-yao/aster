import type { PatternAnalysis } from "../types/dashboard";

export type PatternChatAvailability = "ready" | "stale" | "unavailable";

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
  if (!chatEnabled || !pattern.systemic || !pattern.id || !jobID || !hasEvidenceBuild) return "unavailable";
  return pattern.content_hash ? "ready" : "stale";
}
