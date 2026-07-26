import type { PatternAnalysis } from "../types/dashboard";

export type PatternChatAvailability = "ready" | "stale" | "unavailable";

export function patternChatAvailability(
  pattern: PatternAnalysis,
  jobID: string | undefined,
  hasEvidenceBuild: boolean,
  chatEnabled: boolean,
): PatternChatAvailability {
  if (!chatEnabled || !pattern.systemic || !pattern.id || !jobID || !hasEvidenceBuild) return "unavailable";
  return pattern.content_hash ? "ready" : "stale";
}
