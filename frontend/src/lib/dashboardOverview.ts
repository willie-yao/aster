import type { JobSummary } from "../types/dashboard";

export const MAX_OVERVIEW_PATTERNS = 5;

export type OverviewStatusFilter = "ALL" | "PASSING" | "FLAKY" | "FAILING";

export function countLabel(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

export function overviewHeadline(failingJobs: number, recurringPatterns: number): string {
  return `${countLabel(failingJobs, "failing job")} · ${countLabel(recurringPatterns, "recurring pattern")}`;
}

export function overviewHeadlineForReport(
  failingJobs: number,
  recurringPatterns: number | null,
  loading: boolean,
  failed: boolean,
): string {
  if (loading) return `${countLabel(failingJobs, "failing job")} · recurring patterns loading`;
  if (failed || recurringPatterns === null) {
    return `${countLabel(failingJobs, "failing job")} · recurring patterns unavailable`;
  }
  return overviewHeadline(failingJobs, recurringPatterns);
}

export function attentionSignal(confidence: string, stale: boolean): string {
  return `${confidence} confidence${stale ? " · Last known good" : ""}`;
}

export function orderedDashboardBranches(jobs: JobSummary[]): string[] {
  return Array.from(new Set(jobs.map((job) => job.branch).filter(Boolean))).sort((a, b) => {
    if (a === "main") return -1;
    if (b === "main") return 1;
    const aMatch = a.match(/(\d+)\.(\d+)/);
    const bMatch = b.match(/(\d+)\.(\d+)/);
    if (aMatch && bMatch) {
      const aMajor = Number(aMatch[1]);
      const aMinor = Number(aMatch[2]);
      const bMajor = Number(bMatch[1]);
      const bMinor = Number(bMatch[2]);
      if (aMajor !== bMajor) return bMajor - aMajor;
      return bMinor - aMinor;
    }
    return a.localeCompare(b);
  });
}
