import type { FetchProgressStatus, FetchStatusResponse, FetchStatusState } from "../types/fetchStatus";

export interface FetchStatusPresentation {
  title: string;
  detail: string;
  ariaLabel: string;
  announcement: string;
  severity: "info" | "warning" | "error" | "success";
  determinateTotal: number | null;
  determinateCompleted: number;
}

export interface FetchStatusCompactPresentation {
  label: string;
  ariaLabel: string;
  severity: FetchStatusPresentation["severity"];
}

export type FetchMacroStageID = "fetch" | "analyze" | "patterns" | "publish";
export type FetchMacroStageState =
  | "complete"
  | "active"
  | "pending"
  | "failed"
  | "stale"
  | "interrupted"
  | "cancelled";

export interface FetchMacroStagePresentation {
  id: FetchMacroStageID;
  label: string;
  state: FetchMacroStageState;
  stateLabel: string;
}

const patternFailureLabels: Record<string, string> = {
  ambiguous: "ambiguous response",
  "request-timeout": "request timeout",
  "rate-limited": "rate limited",
  "provider-5xx": "provider failure",
  provider: "provider error",
  "context-headroom": "context headroom unavailable",
  "tools-unsupported": "tools unsupported",
  json: "invalid JSON",
  missing: "missing response",
  schema: "invalid schema",
  builds: "invalid build references",
  cancelled: "cancelled",
  deadline: "deadline exceeded",
  unknown: "unknown",
  multiple: "multiple categories",
};

const phaseLabels: Record<string, string> = {
  setup: "Preparing refresh",
  discovery: "Discovering jobs",
  artifacts: "Fetching runs",
  aggregation: "Building dashboard",
  "analysis-planning": "Planning analyses",
  analysis: "Analyzing failures",
  patterns: "Finalizing patterns",
  publication: "Publishing dashboard",
  "side-effects": "Finishing refresh",
  idle: "Up to date",
  complete: "Up to date",
  failed: "Refresh failed",
  cancelled: "Refresh cancelled",
  interrupted: "Refresh interrupted",
};

const phaseHeadlines: Record<string, string> = {
  ...phaseLabels,
  patterns: "Finalizing recurring patterns",
};

const macroStageDefinitions: Array<{
  id: FetchMacroStageID;
  label: string;
  phases: string[];
}> = [
  { id: "fetch", label: "Fetch data", phases: ["setup", "discovery", "artifacts", "aggregation"] },
  { id: "analyze", label: "Analyze", phases: ["analysis-planning", "analysis"] },
  { id: "patterns", label: "Patterns", phases: ["patterns"] },
  { id: "publish", label: "Publish", phases: ["publication", "side-effects"] },
];

const macroStageStateLabels: Record<FetchMacroStageState, string> = {
  complete: "Complete",
  active: "Active",
  pending: "Pending",
  failed: "Failed",
  stale: "Status stale",
  interrupted: "Interrupted",
  cancelled: "Cancelled",
};

export interface AnalysisProgressBreakdown {
  total: number;
  ready: number;
  reusedFromCache: number;
  compatibleResults: number;
  reused: number;
  exactResultsReused: number;
  sameFailureResultsReused: number;
  sameFailureGroups: number;
  sameFailureCandidates: number;
  potentialTasksSaved: number;
  largestSameFailureGroup: number;
  lateTasksAdopted: number;
  newTasksCreated: number;
  freshAnalysesCompleted: number;
  analyzing: number;
  waiting: number;
  failed: number;
  cancelled: number;
  terminal: number;
}

function nonNegative(value: number | undefined): number {
  return Math.max(0, value ?? 0);
}

export function analysisProgressBreakdown(status: FetchProgressStatus): AnalysisProgressBreakdown {
  const analyses = status.analyses;
  const reusedFromCache = nonNegative(analyses.accepted_cache_hits);
  const compatibleResults = nonNegative(analyses.compatible_results_reused);
  const ready = nonNegative(analyses.completed);
  const failed = nonNegative(analyses.failed);
  const cancelled = nonNegative(analyses.cancelled);
  return {
    total: nonNegative(analyses.logical_total),
    ready,
    reusedFromCache,
    compatibleResults,
    reused: reusedFromCache + compatibleResults,
    exactResultsReused: nonNegative(analyses.exact_results_reused),
    sameFailureResultsReused: nonNegative(analyses.same_failure_results_reused),
    sameFailureGroups: nonNegative(analyses.same_failure_groups),
    sameFailureCandidates: nonNegative(analyses.same_failure_candidates),
    potentialTasksSaved: nonNegative(analyses.potential_tasks_saved),
    largestSameFailureGroup: nonNegative(analyses.largest_same_failure_group),
    lateTasksAdopted: nonNegative(analyses.existing_tasks_adopted),
    newTasksCreated: nonNegative(analyses.new_tasks_created),
    freshAnalysesCompleted: nonNegative(analyses.fresh_analyses_completed),
    analyzing: nonNegative(analyses.running),
    waiting: nonNegative(analyses.queued),
    failed,
    cancelled,
    terminal: ready + failed + cancelled,
  };
}

export function analysisProgressAccessibleDetail(progress: AnalysisProgressBreakdown): string {
  const failureDetail = progress.failed > 0 || progress.cancelled > 0
    ? `, ${progress.failed} failed, ${progress.cancelled} cancelled`
    : "";
  const cohortDetail = progress.potentialTasksSaved > 0
    ? `, ${progress.potentialTasksSaved} potential same-failure Task savings`
    : "";
  return `${progress.ready} of ${progress.total} results ready: ${progress.reused} reused, ${progress.exactResultsReused} exact results reused, ${progress.sameFailureResultsReused} same-failure results reused, ${progress.lateTasksAdopted} existing Tasks adopted, ${progress.freshAnalysesCompleted} newly analyzed, ${progress.analyzing} running, ${progress.waiting} waiting${cohortDetail}${failureDetail}`;
}

function analysisReadyDetail(progress: AnalysisProgressBreakdown): string {
  if (progress.total <= 0) return "Analyses are not planned yet";
  if (progress.ready === progress.total) return `${progress.ready} analyses ready`;
  return `${progress.ready} of ${progress.total} analyses ready`;
}

function activePhaseDetail(status: FetchProgressStatus, analysis: AnalysisProgressBreakdown): string {
  if (status.phase === "artifacts" && status.jobs.total > 0) {
    return `${nonNegative(status.jobs.completed)} of ${nonNegative(status.jobs.total)} jobs checked`;
  }
  if (["analysis", "patterns", "publication", "side-effects"].includes(status.phase) && analysis.total > 0) {
    return analysisReadyDetail(analysis);
  }
  if (status.phase === "analysis-planning" && analysis.total > 0) {
    return `${analysis.total} analyses planned`;
  }
  if (status.jobs.total > 0) {
    return `${nonNegative(status.jobs.total)} jobs in this refresh`;
  }
  return "The current refresh is in progress";
}

function effectivePhase(response: FetchStatusResponse): string | null {
  const status = response.status;
  if (!status) return null;
  if (macroStageDefinitions.some((stage) => stage.phases.includes(status.phase))) return status.phase;
  if (status.failure_category && macroStageDefinitions.some((stage) => stage.phases.includes(status.failure_category ?? ""))) {
    return status.failure_category;
  }
  if (!["pending", "skipped"].includes(status.side_effect_phase) || !["pending", "skipped"].includes(status.publication_phase)) {
    return "publication";
  }
  if (!["pending", "skipped"].includes(status.pattern_phase)) return "patterns";
  if (status.analyses.logical_total > 0) return "analysis";
  return "artifacts";
}

function macroStageState(response: FetchStatusResponse): FetchMacroStageState {
  switch (response.state) {
    case "active":
      return "active";
    case "failed":
      return "failed";
    case "stale":
      return "stale";
    case "interrupted":
      return "interrupted";
    case "cancelled":
      return "cancelled";
    default:
      return "complete";
  }
}

export function fetchStatusHasCompletedPipeline(response: FetchStatusResponse): boolean {
  const status = response.status;
  return response.state === "idle"
    || response.state === "completed"
    || Boolean(status?.outcome === "succeeded" && ["idle", "complete"].includes(status.phase));
}

export function fetchStatusMacroStages(response: FetchStatusResponse): FetchMacroStagePresentation[] {
  const phase = effectivePhase(response);
  const currentIndex = macroStageDefinitions.findIndex((stage) => phase !== null && stage.phases.includes(phase));
  const completedPipeline = fetchStatusHasCompletedPipeline(response);
  if (completedPipeline) {
    return macroStageDefinitions.map((stage) => ({
      id: stage.id,
      label: stage.label,
      state: "complete",
      stateLabel: macroStageStateLabels.complete,
    }));
  }
  const currentState = macroStageState(response);
  return macroStageDefinitions.map((stage, index) => {
    const state: FetchMacroStageState = index < currentIndex
      ? "complete"
      : index === currentIndex ? currentState : "pending";
    return {
      id: stage.id,
      label: stage.label,
      state,
      stateLabel: macroStageStateLabels[state],
    };
  });
}

export function fetchStatusPresentation(response: FetchStatusResponse): FetchStatusPresentation | null {
  const status = response.status;
  if (!response.available || !status) return null;
  const analysis = analysisProgressBreakdown(status);
  const currentPhaseLabel = phaseHeadlines[status.phase] ?? "Refreshing dashboard";
  let title = currentPhaseLabel;
  let detail = activePhaseDetail(status, analysis);
  let severity: FetchStatusPresentation["severity"] = "info";

  switch (response.state) {
    case "idle":
    case "completed":
      title = "Up to date";
      detail = "The latest dashboard is published";
      severity = "success";
      break;
    case "failed": {
      const failedPhase = phaseHeadlines[status.failure_category ?? ""];
      title = "Refresh failed";
      detail = failedPhase ? `Stopped while ${failedPhase.toLowerCase()}` : "The latest refresh did not complete";
      severity = "error";
      break;
    }
    case "stale":
      title = "Status stale";
      detail = status.outcome === "succeeded" && ["idle", "complete"].includes(status.phase)
        ? "The next scheduled refresh check is overdue"
        : `Last reported activity while ${currentPhaseLabel.toLowerCase()}`;
      severity = "warning";
      break;
    case "interrupted":
      title = "Refresh interrupted";
      detail = "The previous refresh stopped before publication completed";
      severity = "warning";
      break;
    case "cancelled":
      title = "Refresh cancelled";
      detail = "The current refresh was cancelled before it completed";
      severity = "warning";
      break;
  }

  let determinateTotal: number | null = null;
  let determinateCompleted = 0;
  if (response.state === "active" && status.phase === "artifacts" && status.jobs.total > 0) {
    determinateTotal = nonNegative(status.jobs.total);
    determinateCompleted = nonNegative(status.jobs.completed);
  } else if (response.state === "active" && status.phase === "analysis" && analysis.total > 0) {
    determinateTotal = analysis.total;
    determinateCompleted = analysis.ready;
  }

  return {
    title,
    detail,
    ariaLabel: `${title}. ${detail}.`,
    announcement: title,
    severity,
    determinateTotal,
    determinateCompleted,
  };
}

export function fetchStatusCompactPresentation(response: FetchStatusResponse): FetchStatusCompactPresentation | null {
  const presentation = fetchStatusPresentation(response);
  const status = response.status;
  if (!presentation || !status) return null;
  const analysis = analysisProgressBreakdown(status);
  let label = presentation.title;
  if (response.state === "active") {
    label = phaseLabels[status.phase] ?? presentation.title;
    if (status.phase === "artifacts" && status.jobs.total > 0) {
      label = `${label} · ${nonNegative(status.jobs.completed)}/${nonNegative(status.jobs.total)}`;
    } else if (status.phase === "analysis" && analysis.total > 0) {
      label = `${label} · ${analysis.ready}/${analysis.total}`;
    }
  }
  return {
    label,
    ariaLabel: `${label}. ${presentation.detail}. Open fetch status details.`,
    severity: presentation.severity,
  };
}

export function shouldShowFetchStatusStrip(response: FetchStatusResponse | null): boolean {
  return Boolean(
    response?.available
    && response.status
    && ["failed", "stale", "interrupted", "cancelled"].includes(response.state),
  );
}

export function patternFailureLabel(category?: string): string | null {
  if (!category) return null;
  return patternFailureLabels[category] ?? "unknown";
}

export function fetchStatusStripKey(response: FetchStatusResponse): string {
  return `${response.status?.pass_id ?? "unknown"}:${response.state}`;
}

export function nextFetchStatusDelay(state: FetchStatusState | undefined, failures: number, baseDelay = 15_000, maxDelay = 120_000): number {
  if (failures > 0) {
    return Math.min(baseDelay * 2 ** Math.min(failures - 1, 3), maxDelay);
  }
  return state === "active" || state === "stale" ? baseDelay : Math.min(baseDelay * 2, maxDelay);
}

export interface PollFetchStatusOptions {
  url: string;
  signal: AbortSignal;
  onStatus: (status: FetchStatusResponse) => void;
  fetcher?: typeof fetch;
  wait?: (delay: number, signal: AbortSignal) => Promise<void>;
  baseDelay?: number;
  maxDelay?: number;
}

export async function pollFetchStatus(options: PollFetchStatusOptions): Promise<void> {
  const fetcher = options.fetcher ?? fetch;
  const wait = options.wait ?? waitForPoll;
  let failures = 0;
  let state: FetchStatusState | undefined;
  while (!options.signal.aborted) {
    try {
      const response = await fetcher(options.url, {
        credentials: "same-origin",
        cache: "no-store",
        signal: options.signal,
      });
      if (response.status === 404 || response.status === 401 || response.status === 403) return;
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const next = await response.json() as FetchStatusResponse;
      if (typeof next.available !== "boolean" || typeof next.state !== "string") {
        throw new Error("invalid fetch status response");
      }
      options.onStatus(next);
      failures = 0;
      state = next.state;
    } catch (error) {
      if (options.signal.aborted || isAbortError(error)) return;
      failures++;
    }
    const delay = nextFetchStatusDelay(state, failures, options.baseDelay, options.maxDelay);
    try {
      await wait(delay, options.signal);
    } catch (error) {
      if (options.signal.aborted || isAbortError(error)) return;
      throw error;
    }
  }
}

function waitForPoll(delay: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, delay);
    const abort = () => {
      window.clearTimeout(timer);
      reject(new DOMException("Polling cancelled", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
  });
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

export function formatFetchTimestamp(value?: string): string {
  if (!value) return "Never";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "Unknown";
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function formatFetchRelativeTime(value?: string, now = Date.now()): string {
  if (!value) return "Never";
  const timestamp = new Date(value).getTime();
  if (Number.isNaN(timestamp)) return "Unknown";
  const delta = timestamp - now;
  const future = delta > 0;
  const seconds = Math.round(Math.abs(delta) / 1000);
  if (seconds < 45) return future ? "in less than a minute" : "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return future ? `in ${minutes}m` : `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return future ? `in ${hours}h` : `${hours}h ago`;
  const days = Math.round(hours / 24);
  return future ? `in ${days}d` : `${days}d ago`;
}

export function nextFetchTime(status: FetchProgressStatus): string | null {
  const candidates = [
    status.next_watch_at ? { label: "Next watch", value: status.next_watch_at } : null,
    status.next_reconcile_at ? { label: "Next reconcile", value: status.next_reconcile_at } : null,
  ].filter((value): value is { label: string; value: string } => value !== null);
  if (candidates.length === 0) return null;
  return candidates.map((candidate) => `${candidate.label}: ${formatFetchTimestamp(candidate.value)}`).join(" · ");
}
