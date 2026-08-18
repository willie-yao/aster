import type { FlakinessReport, JobSummary, LowPassRateEntry, TestFlakiness } from "../types/dashboard";

export const MAX_OVERVIEW_PATTERNS = 5;

// MAX_ATTENTION_ITEMS bounds the test alerts the overview renders across all
// groups combined.
export const MAX_ATTENTION_ITEMS = 10;

export type AttentionGroupKind = "recent" | "persistent" | "flaky" | "lowPassRate";

// AttentionGroup is a discriminated union because the pass-rate group carries
// richer entries and must not borrow the failing styling the classification
// groups use: a test it selects may have already recovered.
export type AttentionGroup =
  | { kind: "recent" | "persistent" | "flaky"; label: string; items: TestFlakiness[] }
  | { kind: "lowPassRate"; label: string; items: LowPassRateEntry[] };

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
  testAlerts: number | null,
  loading: boolean,
  failed: boolean,
): string {
  if (loading) return "recurring patterns loading · test alerts loading";
  if (failed || recurringPatterns === null || testAlerts === null) {
    return "recurring patterns unavailable · test alerts unavailable";
  }
  return `${countLabel(recurringPatterns, "recurring pattern")} · ${countLabel(testAlerts, "test alert")}`;
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
  return `${confidence} confidence${stale ? " · Last successful refresh" : ""}`;
}

function attentionItemKey(test: TestFlakiness): string {
  return `${test.job_id}::${test.test_name}`;
}

// lowPassRateLabel names the pass-rate group after the cutoff that selected it,
// falling back to a generic label when the manifest does not carry one.
export function lowPassRateLabel(threshold: number | undefined): string {
  if (typeof threshold !== "number" || !Number.isFinite(threshold)) return "Low pass rate";
  return `Below ${Math.round(threshold * 1000) / 10}% pass rate`;
}

// passRateSummary states the measured rate and the window it covers, which the
// entry's whole-window fail_rate does not describe once recent_runs narrows it.
export function passRateSummary(entry: LowPassRateEntry): string {
  const percent = Math.round(entry.pass_rate * 1000) / 10;
  return `${percent}% pass rate over ${countLabel(entry.window_runs, "run")}`;
}

// attentionGroupNoun names the items a group hides behind its disclosure.
export function attentionGroupNoun(kind: AttentionGroupKind): readonly [string, string] {
  switch (kind) {
    case "persistent":
      return ["additional persistent failure", "additional persistent failures"];
    case "lowPassRate":
      return ["additional low pass rate test", "additional low pass rate tests"];
    default:
      return ["additional flaky test", "additional flaky tests"];
  }
}

// attentionGroups builds the test-alert groups for the overview. Recent and
// persistent failures lead; flaky tests fill in only when neither is present.
// The optional pass-rate section is appended from the budget those leave behind
// and never repeats a test an earlier group already listed.
export function attentionGroups(
  report: FlakinessReport | null,
  threshold?: number,
): AttentionGroup[] {
  if (!report) return [];
  const broken = report.recently_broken ?? [];
  const persistent = report.persistent_failures ?? [];
  const flaky = report.most_flaky ?? [];
  const groups: AttentionGroup[] = [];
  let remaining = MAX_ATTENTION_ITEMS;

  const pushGroup = (
    kind: "recent" | "persistent" | "flaky",
    label: string,
    source: TestFlakiness[],
  ) => {
    if (source.length === 0 || remaining <= 0) return;
    const items = source.slice(0, remaining);
    groups.push({ kind, label, items });
    remaining -= items.length;
  };

  if (broken.length > 0 || persistent.length > 0) {
    pushGroup("recent", "Recent failures", broken);
    pushGroup("persistent", "Persistent failures", persistent);
  } else {
    pushGroup("flaky", "Flaky tests", flaky);
  }

  const lowPassRate = report.low_pass_rate ?? [];
  if (lowPassRate.length > 0 && remaining > 0) {
    const seen = new Set(groups.flatMap((group) => group.items.map(attentionItemKey)));
    const items = lowPassRate
      .filter((entry) => !seen.has(attentionItemKey(entry)))
      .slice(0, remaining);
    if (items.length > 0) {
      groups.push({ kind: "lowPassRate", label: lowPassRateLabel(threshold), items });
      remaining -= items.length;
    }
  }
  return groups;
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
