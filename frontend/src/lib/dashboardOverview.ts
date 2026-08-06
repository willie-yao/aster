import type { JobSummary } from "../types/dashboard";

export const MAX_OVERVIEW_PATTERNS = 5;

export type OverviewStatusFilter = "ALL" | "PASSING" | "FLAKY" | "FAILING";

export interface OverviewHistoryState {
  additionalOpen: boolean;
  resolvedOpen: boolean;
  expandedGroups: Record<string, boolean>;
  scrollY: number;
}

const defaultOverviewHistoryState: OverviewHistoryState = {
  additionalOpen: false,
  resolvedOpen: false,
  expandedGroups: {},
  scrollY: 0,
};

function recordValue(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null ? value as Record<string, unknown> : null;
}

export function overviewStatusFromParam(value: string | null): OverviewStatusFilter {
  const normalized = value?.trim().toUpperCase();
  return normalized === "PASSING" || normalized === "FLAKY" || normalized === "FAILING"
    ? normalized
    : "ALL";
}

export function overviewBranchFromParam(value: string | null, branches: string[]): string {
  const normalized = value?.trim();
  return normalized && branches.includes(normalized) ? normalized : "ALL";
}

export function withOverviewFilters(
  current: URLSearchParams,
  status: OverviewStatusFilter,
  branch: string,
): URLSearchParams {
  const next = new URLSearchParams(current);
  if (status === "ALL") next.delete("status");
  else next.set("status", status.toLowerCase());
  if (branch === "ALL") next.delete("branch");
  else next.set("branch", branch);
  return next;
}

export function readOverviewHistoryState(historyState: unknown): OverviewHistoryState {
  const root = recordValue(historyState);
  const userState = recordValue(root?.usr);
  const overview = recordValue(userState?.overview);
  const groups = recordValue(overview?.expandedGroups);
  const expandedGroups = Object.fromEntries(
    Object.entries(groups ?? {}).filter((entry): entry is [string, boolean] => typeof entry[1] === "boolean"),
  );
  const scrollY = typeof overview?.scrollY === "number" && Number.isFinite(overview.scrollY)
    ? Math.max(0, overview.scrollY)
    : 0;
  return {
    additionalOpen: typeof overview?.additionalOpen === "boolean"
      ? overview.additionalOpen
      : defaultOverviewHistoryState.additionalOpen,
    resolvedOpen: typeof overview?.resolvedOpen === "boolean"
      ? overview.resolvedOpen
      : defaultOverviewHistoryState.resolvedOpen,
    expandedGroups,
    scrollY,
  };
}

export function mergeOverviewHistoryState(
  historyState: unknown,
  patch: Partial<OverviewHistoryState>,
): Record<string, unknown> {
  const root = recordValue(historyState) ?? {};
  const userState = recordValue(root.usr) ?? {};
  const current = readOverviewHistoryState(root);
  const next: OverviewHistoryState = {
    ...current,
    ...patch,
    expandedGroups: patch.expandedGroups ?? current.expandedGroups,
  };
  return { ...root, usr: { ...userState, overview: next } };
}

export function persistOverviewHistoryState(patch: Partial<OverviewHistoryState>) {
  if (typeof window === "undefined") return;
  window.history.replaceState(mergeOverviewHistoryState(window.history.state, patch), "");
}

export function countLabel(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

export function needsAttentionSummary(
  recurringPatterns: number | null,
  activeItems: number | null,
  loading: boolean,
  failed: boolean,
): string {
  if (loading) return "recurring patterns loading · active items loading";
  if (failed || recurringPatterns === null || activeItems === null) {
    return "recurring patterns unavailable · active items unavailable";
  }
  return `${countLabel(recurringPatterns, "recurring pattern")} · ${countLabel(activeItems, "active item")}`;
}

export function disclosureLabel(
  open: boolean,
  count: number,
  singular: string,
  plural = `${singular}s`,
): string {
  return `${open ? "Hide" : "Show"} ${countLabel(count, singular, plural)}`;
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
