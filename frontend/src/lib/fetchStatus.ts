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
  quiet: boolean;
}

export interface FetchStatusPreferenceStorage {
  getItem: (key: string) => string | null;
  setItem: (key: string, value: string) => void;
}

export interface FetchStatusPreferenceScope {
  readonly localStorage: FetchStatusPreferenceStorage;
}

export const FETCH_STATUS_IDLE_COMPACT_KEY = "prow-ai-dashboard.fetch-status.idle-compact";

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
  setup: "Setup",
  discovery: "Discovery",
  artifacts: "Artifacts",
  aggregation: "Aggregation",
  "analysis-planning": "Analysis planning",
  analysis: "Analysis",
  patterns: "Patterns",
  publication: "Publication",
  "side-effects": "Side effects",
  idle: "Idle",
  complete: "Complete",
  failed: "Failed",
  cancelled: "Cancelled",
  interrupted: "Interrupted",
};

export interface AnalysisProgressBreakdown {
  total: number;
  ready: number;
  reusedFromCache: number;
  compatibleResults: number;
  reused: number;
  exactResultsReused: number;
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
  return `${progress.ready} of ${progress.total} results ready: ${progress.reused} reused, ${progress.exactResultsReused} existing results adopted, ${progress.freshAnalysesCompleted} newly analyzed, ${progress.analyzing} running, ${progress.waiting} waiting${failureDetail}`;
}

export function analysisProgressStripDetail(progress: AnalysisProgressBreakdown): string {
  const failureDetail = progress.failed > 0 || progress.cancelled > 0
    ? ` · ${progress.failed} failed · ${progress.cancelled} cancelled`
    : "";
  return `${progress.reused} reused · ${progress.exactResultsReused} adopted · ${progress.freshAnalysesCompleted} new · ${progress.analyzing} analyzing · ${progress.waiting} waiting${failureDetail}`;
}

export function fetchStatusPresentation(response: FetchStatusResponse): FetchStatusPresentation | null {
  const status = response.status;
  if (!response.available || !status) return null;
  const phase = phaseLabels[status.phase] ?? "Fetch";
  const analysis = analysisProgressBreakdown(status);
  const hasAnalysis = analysis.total > 0;
  const logicalDetail = hasAnalysis
    ? analysisProgressStripDetail(analysis)
    : `${nonNegative(status.jobs.completed)} of ${nonNegative(status.jobs.total)} jobs checked`;
  const state = response.state;
  let title = state === "active" && hasAnalysis
    ? `${analysis.ready} of ${analysis.total} results ready`
    : `Fetch in progress: ${phase}`;
  let severity: FetchStatusPresentation["severity"] = "info";
  if (state === "idle") {
    title = "Fetch idle";
  } else if (state === "completed") {
    title = "Fetch complete";
    severity = "success";
  } else if (state === "stale") {
    title = `Fetch status stale: ${phase}`;
    severity = "warning";
  } else if (state === "interrupted") {
    title = "Previous fetch interrupted";
    severity = "warning";
  } else if (state === "failed") {
    const failedPhase = phaseLabels[status.failure_category ?? ""] ?? phase;
    title = `Fetch failed: ${failedPhase}`;
    severity = "error";
  } else if (state === "cancelled") {
    title = "Fetch cancelled";
    severity = "warning";
  }
  const ariaProgress = hasAnalysis
    ? analysisProgressAccessibleDetail(analysis)
    : logicalDetail;
  const ariaState = state === "active" ? `Fetch in progress: ${phase}` : title;
  const determinateTotal = hasAnalysis
    ? analysis.total
    : status.jobs.total > 0 ? nonNegative(status.jobs.total) : null;
  const determinateCompleted = hasAnalysis ? analysis.terminal : nonNegative(status.jobs.completed);
  return {
    title,
    detail: logicalDetail,
    ariaLabel: `${ariaState}. ${ariaProgress}.`,
    announcement: ariaState,
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
  let label = "Fetch";
  let quiet = false;
  let severity = presentation.severity;
  switch (response.state) {
    case "active": {
      const phase = phaseLabels[status.phase] ?? "Fetch";
      label = analysis.total > 0 ? `${analysis.ready}/${analysis.total} ready` : phase;
      break;
    }
    case "idle":
      label = "Idle";
      quiet = true;
      severity = "success";
      break;
    case "completed":
      label = "Complete";
      quiet = true;
      severity = "success";
      break;
    case "failed":
      label = "Fetch failed";
      break;
    case "stale":
      label = "Status stale";
      break;
    case "interrupted":
      label = "Interrupted";
      break;
    case "cancelled":
      label = "Cancelled";
      break;
  }
  return {
    label,
    ariaLabel: `${presentation.ariaLabel} Open fetch status details.`,
    severity,
    quiet,
  };
}

export function patternFailureLabel(category?: string): string | null {
  if (!category) return null;
  return patternFailureLabels[category] ?? "unknown";
}

export function fetchStatusStripKey(response: FetchStatusResponse): string {
  return `${response.status?.pass_id ?? "unknown"}:${response.state}`;
}

export function resolveFetchStatusPreferenceStorage(scope?: FetchStatusPreferenceScope | null): FetchStatusPreferenceStorage | null {
  if (!scope) return null;
  try {
    return scope.localStorage;
  } catch {
    return null;
  }
}

export function readFetchStatusIdleCompact(storage?: FetchStatusPreferenceStorage | null): boolean {
  if (!storage) return false;
  try {
    return storage.getItem(FETCH_STATUS_IDLE_COMPACT_KEY) === "true";
  } catch {
    return false;
  }
}

export function writeFetchStatusIdleCompact(value: boolean, storage?: FetchStatusPreferenceStorage | null): void {
  if (!storage) return;
  try {
    storage.setItem(FETCH_STATUS_IDLE_COMPACT_KEY, value ? "true" : "false");
  } catch {
    // The preference is optional when storage is unavailable.
  }
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

export function nextFetchTime(status: FetchProgressStatus): string | null {
  const candidates = [
    status.next_watch_at ? { label: "Next watch", value: status.next_watch_at } : null,
    status.next_reconcile_at ? { label: "Next reconcile", value: status.next_reconcile_at } : null,
  ].filter((value): value is { label: string; value: string } => value !== null);
  if (candidates.length === 0) return null;
  return candidates.map((candidate) => `${candidate.label}: ${formatFetchTimestamp(candidate.value)}`).join(" · ");
}
