import type {
  AttributionVerdict,
  PullRequestCIState,
  PullRequestCheck,
  PullRequestFailure,
  PullRequestSummary,
} from "../types/pullRequests";

// State filter for the pull request ledger. ALL disables filtering.
export type PullRequestStateFilter = "ALL" | PullRequestCIState;

const stateFilters: PullRequestStateFilter[] = [
  "ALL",
  "FAILING",
  "PENDING",
  "PASSING",
  "UNKNOWN",
];

// Display state of one check. Derived rather than stored so a build that has
// not finished is never presented as a pass.
export type CheckState = "FAILING" | "RUNNING" | "PASSING";

export function pullRequestStateFromParam(raw: string | null): PullRequestStateFilter {
  const candidate = (raw ?? "").toUpperCase();
  return stateFilters.find((state) => state === candidate) ?? "ALL";
}

export function withPullRequestState(
  params: URLSearchParams,
  state: PullRequestStateFilter,
): URLSearchParams {
  const next = new URLSearchParams(params);
  if (state === "ALL") {
    next.delete("state");
  } else {
    next.set("state", state);
  }
  return next;
}

export function filterPullRequests(
  pulls: PullRequestSummary[],
  state: PullRequestStateFilter,
): PullRequestSummary[] {
  return state === "ALL" ? pulls : pulls.filter((pull) => pull.ci_state === state);
}

export type PullRequestStateCounts = Record<PullRequestCIState, number> & { ALL: number };

export function pullRequestStateCounts(pulls: PullRequestSummary[]): PullRequestStateCounts {
  const counts: PullRequestStateCounts = {
    ALL: pulls.length,
    FAILING: 0,
    PENDING: 0,
    PASSING: 0,
    UNKNOWN: 0,
  };
  for (const pull of pulls) {
    if (pull.ci_state in counts) counts[pull.ci_state] += 1;
  }
  return counts;
}

// orderPullRequests puts pull requests that need attention first, then the
// most recently updated, so the ledger leads with signal.
export function orderPullRequests(pulls: PullRequestSummary[]): PullRequestSummary[] {
  const rank: Record<PullRequestCIState, number> = {
    FAILING: 0,
    PENDING: 1,
    PASSING: 2,
    UNKNOWN: 3,
  };
  return [...pulls].sort((a, b) => {
    const byState = (rank[a.ci_state] ?? 9) - (rank[b.ci_state] ?? 9);
    if (byState !== 0) return byState;
    const byUpdated = Date.parse(b.updated_at) - Date.parse(a.updated_at);
    if (!Number.isNaN(byUpdated) && byUpdated !== 0) return byUpdated;
    return b.number - a.number;
  });
}

export function checkState(check: PullRequestCheck): CheckState {
  if (!check.finished) return "RUNNING";
  return check.passed ? "PASSING" : "FAILING";
}

// checkStatusLabel maps a check to the vocabulary StatusChip already renders
// across the dashboard.
export function checkStatusLabel(check: PullRequestCheck): string {
  switch (checkState(check)) {
    case "RUNNING":
      return "Running";
    case "FAILING":
      return "Failed";
    default:
      return "Passed";
  }
}

// shortSHA abbreviates a commit for display without implying it is a full ref.
export function shortSHA(sha: string | undefined): string {
  const trimmed = (sha ?? "").trim();
  return trimmed.length > 7 ? trimmed.slice(0, 7) : trimmed;
}

// checkSummaryLine describes what a failing check reported, distinguishing a
// job that failed with no failing case from one that failed several.
export function checkSummaryLine(check: PullRequestCheck): string {
  const state = checkState(check);
  if (state === "RUNNING") return "Still running";
  if (state === "PASSING") return "Passed";
  const failed = check.tests_failed ?? 0;
  if (failed === 0) return "Failed without reporting a failing test";
  const shown = check.failures?.length ?? 0;
  if (check.failures_truncated && shown > 0) {
    return `${failed} failing tests, showing the first ${shown}`;
  }
  return failed === 1 ? "1 failing test" : `${failed} failing tests`;
}

// staleCheckCount reports how many checks tested an older head than the pull
// request's current one.
export function staleCheckCount(checks: PullRequestCheck[]): number {
  return checks.filter((check) => check.stale).length;
}

// attributionLabel is the short chip text for a verdict. The wording avoids
// asserting that a pull request caused a failure, because the deterministic
// pass compares observations and cannot establish causation.
export function attributionLabel(verdict: AttributionVerdict): string {
  switch (verdict) {
    case "pre_existing":
      return "Already failing on base";
    case "widespread":
      return "Not this PR";
    case "known_flake":
      return "Known flake";
    case "touches_changed_code":
      return "Touches changed code";
    case "unexplained":
      return "Needs investigation";
    default:
      return "Inconclusive";
  }
}

// attributionTone maps a verdict to the palette the dashboard already uses for
// severity. Verdicts that rule the pull request out are informational.
export function attributionTone(
  verdict: AttributionVerdict,
): "info" | "warning" | "error" | "default" {
  switch (verdict) {
    case "pre_existing":
    case "widespread":
      return "info";
    case "known_flake":
      return "warning";
    case "touches_changed_code":
    case "unexplained":
      return "error";
    default:
      return "default";
  }
}

// needsInvestigation reports whether a failure was left for a human, which is
// what the pull request ledger counts.
export function needsInvestigation(failure: PullRequestFailure): boolean {
  const verdict = failure.attribution?.verdict;
  return (
    verdict === undefined ||
    verdict === "unexplained" ||
    verdict === "touches_changed_code" ||
    verdict === "inconclusive"
  );
}

// unexplainedCount totals the failures across checks that the baseline could
// not rule out.
export function unexplainedCount(checks: PullRequestCheck[]): number {
  let total = 0;
  for (const check of checks) {
    for (const failure of check.failures ?? []) {
      if (needsInvestigation(failure)) total += 1;
    }
  }
  return total;
}
