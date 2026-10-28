import type {
  FlakinessReport,
  JobSummary,
  LowPassRateEntry,
  PatternAnalysis,
  PatternRefreshStatus,
  ResolvedEntry,
  ResolvedState,
  RunSummary,
  TestFlakiness,
} from "../types/dashboard";
import { patternLifecycleActive } from "./actionEligibility.js";

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

// Reliability buckets come from overall_status, the last 10 runs. They use
// their own names so "Failed" and "Passed" stay reserved for a run's result.
export type OverviewStatusFilter = "ALL" | "HEALTHY" | "FLAKY" | "UNRELIABLE" | "LATEST_FAILED";

export const reliabilityLabels = {
  HEALTHY: "Healthy",
  FLAKY: "Flaky",
  UNRELIABLE: "Unreliable",
} as const;

const reliabilityByOverallStatus: Record<JobSummary["overall_status"], keyof typeof reliabilityLabels> = {
  PASSING: "HEALTHY",
  FLAKY: "FLAKY",
  FAILING: "UNRELIABLE",
};

export function jobMatchesStatusFilter(job: JobSummary, filter: OverviewStatusFilter): boolean {
  if (filter === "ALL") return true;
  if (filter === "LATEST_FAILED") return job.current_status === "FAILING";
  return reliabilityByOverallStatus[job.overall_status] === filter;
}

// Run words describe one run's result, as opposed to the reliability buckets.
export function runStatusLabel(status: JobSummary["current_status"]): string {
  switch (status) {
    case "PASSING":
      return "Passed";
    case "FAILING":
      return "Failed";
    case "RUNNING":
      return "Running";
    default:
      return "Unknown";
  }
}

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
  const normalized = value?.trim().toUpperCase().replace(/-/g, "_");
  return normalized === "HEALTHY" || normalized === "FLAKY" || normalized === "UNRELIABLE" || normalized === "LATEST_FAILED"
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
  else next.set("status", status.toLowerCase().replace(/_/g, "-"));
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

// patternCountOutdated reports whether a pattern's builds_analyzed describes a
// wider window than the current one. All three conditions matter: a current
// refresh rebuilt the count from the live window, a pattern with no shared builds
// leaves evidence_available false no matter what is still present, and an
// available pattern still holds every build it correlated.
export function patternCountOutdated(
  pattern: Pick<PatternAnalysis, "shared_builds">,
  refreshStatus?: PatternRefreshStatus,
): boolean {
  if (!refreshStatus || refreshStatus.state === "current") return false;
  if ((pattern.shared_builds?.length ?? 0) === 0) return false;
  return refreshStatus.evidence_available === false;
}

// buildsAnalyzedLabel describes how many builds a pattern correlated. The count
// is fixed when the pattern is generated, so a pattern being retained keeps
// reporting the window it was correlated in even after those builds age out.
export function buildsAnalyzedLabel(
  pattern: Pick<PatternAnalysis, "builds_analyzed" | "shared_builds">,
  refreshStatus?: PatternRefreshStatus,
): string {
  const label = countLabel(pattern.builds_analyzed, "build");
  return patternCountOutdated(pattern, refreshStatus) ? `${label} (earlier window)` : label;
}

function recurringBuildGroups(
  pattern: Pick<PatternAnalysis, "causal_groups" | "shared_builds">,
): string[][] {
  const groups = (pattern.causal_groups ?? [])
    .map((group) => group.builds)
    .filter((builds) => builds.length > 1);
  if (groups.length > 0) return groups;
  return pattern.shared_builds?.length ? [pattern.shared_builds] : [];
}

export function patternRecurringBuildCount(
  pattern: Pick<PatternAnalysis, "causal_groups" | "shared_builds">,
): number {
  return recurringBuildGroups(pattern).reduce(
    (largest, builds) => Math.max(largest, new Set(builds).size),
    0,
  );
}

// currentPatternFailureStreak counts newest completed failures attributed to
// one recurring cause. Pending runs neither extend nor break the streak.
export function currentPatternFailureStreak(
  pattern: Pick<PatternAnalysis, "causal_groups" | "shared_builds" | "lifecycle">,
  job?: Pick<JobSummary, "recent_runs">,
): number {
  if (!job) return 0;
  let longest = 0;
  const latestRecurrence = pattern.lifecycle?.latest_recurrence_build_id;
  if (latestRecurrence) {
    const latestCompleted = job.recent_runs.find((run) => run.result !== "PENDING");
    if (latestCompleted?.build_id === latestRecurrence && !latestCompleted.passed) longest = 1;
  }
  for (const group of recurringBuildGroups(pattern)) {
    const builds = new Set(group);
    let streak = 0;
    for (const run of job.recent_runs) {
      if (run.result === "PENDING") continue;
      if (run.passed || !builds.has(run.build_id)) break;
      streak++;
    }
    longest = Math.max(longest, streak);
  }
  return longest;
}

function patternConfidenceRank(confidence: PatternAnalysis["confidence"]): number {
  switch (confidence) {
    case "high": return 3;
    case "medium": return 2;
    case "low": return 1;
  }
}

// rankRecurringPatterns leads with patterns that are failing now from the same
// cause, then with active patterns that have the least recovery evidence.
export function rankRecurringPatterns(
  patterns: PatternAnalysis[],
  jobsByID: Record<string, JobSummary>,
): PatternAnalysis[] {
  return [...patterns].sort((a, b) => {
    const aStreak = currentPatternFailureStreak(a, a.job_id ? jobsByID[a.job_id] : undefined);
    const bStreak = currentPatternFailureStreak(b, b.job_id ? jobsByID[b.job_id] : undefined);
    if (aStreak !== bStreak) return bStreak - aStreak;

    if (aStreak === 0) {
      const aRecovery = a.lifecycle?.recovery_streak ?? 0;
      const bRecovery = b.lifecycle?.recovery_streak ?? 0;
      if (aRecovery !== bRecovery) return aRecovery - bRecovery;
    }

    const confidence = patternConfidenceRank(b.confidence) - patternConfidenceRank(a.confidence);
    if (confidence !== 0) return confidence;

    const recurringBuilds = patternRecurringBuildCount(b) - patternRecurringBuildCount(a);
    if (recurringBuilds !== 0) return recurringBuilds;
    if (a.builds_analyzed !== b.builds_analyzed) return b.builds_analyzed - a.builds_analyzed;
    return (a.job_id ?? a.subject).localeCompare(b.job_id ?? b.subject);
  });
}

export function patternEvidenceLabel(
  pattern: Pick<PatternAnalysis, "builds_analyzed" | "causal_groups" | "shared_builds" | "confidence">,
  refreshStatus?: PatternRefreshStatus,
): string {
  const recurringBuilds = patternRecurringBuildCount(pattern);
  const parts = [
    recurringBuilds > 0
      ? `${recurringBuilds} of ${countLabel(pattern.builds_analyzed, "failure")}`
      : countLabel(pattern.builds_analyzed, "analyzed failure"),
  ];
  if (pattern.confidence !== "high") parts.push(`${pattern.confidence} confidence`);
  if (refreshStatus && refreshStatus.state !== "current") {
    const refreshed = new Date(refreshStatus.last_successful_at ?? "");
    parts.push(Number.isFinite(refreshed.getTime())
      ? `as of ${refreshed.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" })}`
      : "earlier refresh");
  }
  return parts.join(" · ");
}

export type RunChange =
  | { kind: "broke" | "recovered"; changed: RunSummary; before: RunSummary }
  | { kind: "noPass" | "noFailure"; since: RunSummary };

// runChange finds where the newest streak of finished runs began and the run
// just before it, which has the opposite outcome. Runs are newest first, and
// a run still in progress has no outcome yet.
export function runChange(runs: RunSummary[]): RunChange | null {
  const settled = runs.filter((run) => run.result !== "PENDING");
  if (settled.length === 0) return null;
  const failing = !settled[0].passed;
  const boundary = settled.findIndex((run) => run.passed === failing);
  if (boundary === -1) return { kind: failing ? "noPass" : "noFailure", since: settled[settled.length - 1] };
  return { kind: failing ? "broke" : "recovered", changed: settled[boundary - 1], before: settled[boundary] };
}

export function runDateLabel(run: RunSummary): string {
  const date = new Date(run.timestamp);
  return Number.isFinite(date.getTime())
    ? date.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" })
    : `run ${run.build_id}`;
}

export function patternRecurrenceLabel(
  pattern: Pick<PatternAnalysis, "lifecycle">,
): string | null {
  const lifecycle = pattern.lifecycle;
  if (!lifecycle?.latest_recurrence_build_id) return null;
  const started = new Date(lifecycle.latest_recurrence_started ?? "");
  if (!Number.isFinite(started.getTime())) return "Latest recurrence";
  const date = started.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
  return `Recurred ${date}`;
}

export function needsAttentionSummary(
  recurringPatterns: number | null,
  testAlerts: number | null,
  loading: boolean,
  failed: boolean,
  sharedFailures = 0,
): string {
  if (loading) return "recurring patterns loading · test alerts loading";
  if (failed || recurringPatterns === null || testAlerts === null) {
    return "recurring patterns unavailable · test alerts unavailable";
  }
  const summary = `${countLabel(recurringPatterns, "recurring pattern")} · ${countLabel(testAlerts, "test alert")}`;
  return sharedFailures > 0
    ? `${summary} · ${countLabel(sharedFailures, "shared PR failure")}`
    : summary;
}

export function disclosureLabel(
  open: boolean,
  count: number,
  singular: string,
  plural = `${singular}s`,
): string {
  return `${open ? "Hide" : "Show"} ${countLabel(count, singular, plural)}`;
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
// Listed patterns cover matching alerts before the shared item budget applies.
// The optional pass-rate section is appended from the budget those leave behind
// and never repeats a test an earlier group already listed.
export function attentionGroups(
  report: FlakinessReport | null,
  threshold?: number,
  displayedPatterns: PatternAnalysis[] = [],
): AttentionGroup[] {
  if (!report) return [];
  const uncovered = (test: TestFlakiness) => {
    const buildID = test.last_failure?.build_id;
    if (!buildID) return true;
    return !displayedPatterns.some((pattern) =>
      pattern.job_id === test.job_id &&
      patternLifecycleActive(pattern.lifecycle) &&
      (pattern.shared_builds?.includes(buildID) ||
        pattern.causal_groups?.some((group) => group.builds.includes(buildID))),
    );
  };
  const broken = (report.recently_broken ?? []).filter(uncovered);
  const persistent = (report.persistent_failures ?? []).filter(uncovered);
  const flaky = (report.most_flaky ?? []).filter(uncovered);
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
      .filter((entry) => uncovered(entry) && !seen.has(attentionItemKey(entry)))
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

// patternFullyResolved reports whether every failure a pattern represents has
// been acknowledged. A pattern-level resolution covers it outright; otherwise
// each of its causes must be resolved individually, so acknowledging one cause
// of four leaves the pattern in the active view with three causes still to
// answer. A pattern with no causal groups can only be resolved at pattern
// scope.
export function patternFullyResolved(
  pattern: PatternAnalysis,
  resolved: ResolvedState,
): boolean {
  if (pattern.id && resolved.resolved[pattern.id]) return true;
  const groups = pattern.causal_groups ?? [];
  return (
    groups.length > 0 &&
    groups.every((group) => Boolean(group.signature && resolved.causes[group.signature]))
  );
}

// unlistedPatternResolutions selects pattern resolutions whose pattern has left
// the active recurring set, paired with their id. Such a marker is retained on
// purpose (correlation can miss for a single pass, and dropping it would return
// the pattern to the active view unbidden), but the overview stops showing it,
// so the resolved-failures disclosure offers it here.
//
// This covers a pattern that aged out entirely and one whose lifecycle moved to
// recovered, observing, or verified fixed. The overview reads only the
// recurring set, which is already filtered to active patterns, so it cannot
// distinguish the two, and a lifecycle-inactive pattern keeps its own banner
// where Reopen is also offered.
//
// Reopening is the only thing a viewer can do with one, so they are selected
// only where that is possible. A report that has not loaded yields none: it
// cannot tell an unlisted pattern from an unread one.
export function unlistedPatternResolutions(
  report: FlakinessReport | null,
  resolved: ResolvedState,
  canRestore: boolean,
): [string, ResolvedEntry][] {
  if (!report || !canRestore) return [];
  const published = new Set(
    (report.recurring_patterns ?? []).map((pattern) => pattern.id).filter(Boolean),
  );
  return Object.entries(resolved.resolved ?? {}).filter(([id]) => !published.has(id));
}

// unlistedCauseResolutions selects resolved causes that no published pattern
// still shows, paired with their signature. A cause leaves the published set
// when its builds age out of the window, and its resolution is retained for the
// same reason a pattern's is, so this disclosure is the only place left to
// reopen one.
export function unlistedCauseResolutions(
  report: FlakinessReport | null,
  resolved: ResolvedState,
  canRestore: boolean,
): [string, ResolvedEntry][] {
  if (!report || !canRestore) return [];
  const published = new Set(
    (report.recurring_patterns ?? [])
      .flatMap((pattern) => pattern.causal_groups ?? [])
      .map((group) => group.signature)
      .filter(Boolean),
  );
  return Object.entries(resolved.causes ?? {}).filter(
    ([signature]) => !published.has(signature),
  );
}
