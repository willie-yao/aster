import type { FetchProgressStatus, FetchStatusResponse, FetchStatusState } from "../types/fetchStatus";

export interface FetchStatusPresentation {
  title: string;
  detail: string;
  ariaLabel: string;
  severity: "info" | "warning" | "error" | "success";
  determinateTotal: number | null;
  determinateCompleted: number;
}

const patternFailureLabels: Record<string, string> = {
  ambiguous: "ambiguous response",
  "request-timeout": "request timeout",
  "rate-limited": "rate limited",
  "provider-5xx": "provider failure",
  provider: "provider error",
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

export function fetchStatusPresentation(response: FetchStatusResponse): FetchStatusPresentation | null {
  const status = response.status;
  if (!response.available || !status) return null;
  const phase = phaseLabels[status.phase] ?? "Fetch";
  const analysesDone = status.analyses.completed + status.analyses.failed + status.analyses.cancelled;
  const logicalDetail = status.analyses.logical_total > 0
    ? `${analysesDone} of ${status.analyses.logical_total} analyses complete, ${status.analyses.running} running, ${status.analyses.queued} queued`
    : `${status.jobs.completed} of ${status.jobs.total} jobs checked`;
  const attemptDetail = status.analyses.task_attempts > 0 ? `, ${status.analyses.task_attempts} Task attempts` : "";
  const retryDetail = status.analyses.retries > 0 ? `, ${status.analyses.retries} retries` : "";
  const patternAttempts = status.patterns?.attempts ?? 0;
  const patternRetries = status.patterns?.retries ?? 0;
  const patternAttemptDetail = patternAttempts > 0 ? `, ${patternAttempts} pattern ${patternAttempts === 1 ? "attempt" : "attempts"}` : "";
  const patternRetryDetail = patternRetries > 0 ? `, ${patternRetries} pattern ${patternRetries === 1 ? "retry" : "retries"}` : "";
  const patternFailureDetail = status.patterns?.failure_category
    ? `, pattern failure: ${patternFailureLabels[status.patterns.failure_category] ?? "unknown"}`
    : "";
  const state = response.state;
  let title = `Fetch in progress: ${phase}`;
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
  const detail = `${logicalDetail}${attemptDetail}${retryDetail}${patternAttemptDetail}${patternRetryDetail}${patternFailureDetail}`;
  const determinateTotal = status.analyses.logical_total > 0
    ? status.analyses.logical_total
    : status.jobs.total > 0 ? status.jobs.total : null;
  const determinateCompleted = status.analyses.logical_total > 0 ? analysesDone : status.jobs.completed;
  return {
    title,
    detail,
    ariaLabel: `${title}. ${detail}.`,
    severity,
    determinateTotal,
    determinateCompleted,
  };
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
