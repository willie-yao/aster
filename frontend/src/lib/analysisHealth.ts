import type { AnalysisTrace, AnalysisTraceEvent } from "../types/traces";

/**
 * Health severity for one analysis, ordered most to least urgent.
 *
 * failed   the analysis produced no usable verdict
 * degraded a verdict was published, but a gate, budget, or provider misbehaved
 * retried  quality gates fired and the retry recovered
 * healthy  the analysis completed without intervention
 */
export type AnalysisHealthSeverity = "failed" | "degraded" | "retried" | "healthy";

export const analysisHealthSeverities: readonly AnalysisHealthSeverity[] = [
  "failed",
  "degraded",
  "retried",
  "healthy",
] as const;

export const analysisHealthSeverityLabels: Record<AnalysisHealthSeverity, string> = {
  failed: "Failed",
  degraded: "Degraded",
  retried: "Recovered",
  healthy: "Healthy",
};

export const analysisHealthSeverityDescriptions: Record<AnalysisHealthSeverity, string> = {
  failed: "No usable analysis was published for these failures.",
  degraded: "An analysis was published, but a quality gate, budget, or provider misbehaved.",
  retried: "Quality gates fired and the retry recovered a usable analysis.",
  healthy: "These analyses completed without intervention.",
};

export interface AnalysisHealthVerdict {
  severity: AnalysisHealthSeverity;
  reasons: string[];
  modelRequests: number;
  toolCalls: number;
  toolErrors: number;
  evidenceBytes: number;
}

const failedOutcomes = new Set(["error", "unavailable", "failed", "cancelled", "rejected"]);
const finalizeProblems = new Set(["empty", "error", "headroom_denied"]);
const cacheProblems = new Set(["rejected", "error", "policy_unavailable"]);
const toolBudgetProblems = new Set(["model_budget_exhausted", "gcs_budget_exhausted", "disabled"]);
const critiqueRetryProblems = new Set(["tool_turn_error", "unparseable"]);
const headroomProblems: Record<string, string> = {
  unavailable: "Ran out of context headroom",
  best_draft: "Fell back to the best draft: no context headroom",
  retry_denied: "Retry denied: no context headroom",
};

function phrase(value?: string): string {
  return (value ?? "").replaceAll("_", " ").trim();
}

function modelRequestFailed(event: AnalysisTraceEvent): boolean {
  return Boolean(event.error_code) || (event.http_status ?? 0) >= 400;
}

/**
 * Classifies one trace into a health severity with the reasons that earned it.
 * Rules run in fixed order so the reason list is deterministic.
 */
export function analysisHealthVerdict(trace: AnalysisTrace): AnalysisHealthVerdict {
  const failures: string[] = [];
  const degradations: string[] = [];
  const retries: string[] = [];

  if (failedOutcomes.has(trace.outcome)) {
    failures.push(
      trace.error_code
        ? `Analysis ${phrase(trace.outcome)}: ${phrase(trace.error_code)}`
        : `Analysis ${phrase(trace.outcome)}`,
    );
  } else if (trace.error_code) {
    failures.push(`Analysis error: ${phrase(trace.error_code)}`);
  }

  if (trace.truncated) degradations.push("Trace recording truncated");

  let modelRequests = 0;
  let toolCalls = 0;
  let toolErrors = 0;
  let evidenceBytes = 0;
  let pendingStructured: string[] = [];
  const recoveredStructured: string[] = [];

  for (const event of trace.events) {
    if (event.kind === "model_request") {
      modelRequests++;
      if (modelRequestFailed(event)) {
        degradations.push(
          event.error_code
            ? `Model request error: ${phrase(event.error_code)}`
            : `Model request returned HTTP ${event.http_status}`,
        );
      }
      continue;
    }
    if (event.kind === "tool_call") {
      toolCalls++;
      evidenceBytes += event.bytes ?? 0;
      // A single tool error is normal exploration, so it is counted but does
      // not by itself degrade the analysis. Budget exhaustion does.
      if (event.outcome === "error") toolErrors++;
      else if (event.outcome && toolBudgetProblems.has(event.outcome)) {
        degradations.push(`Evidence gathering stopped: ${phrase(event.outcome)}`);
      }
      continue;
    }
    switch (event.kind) {
      case "floor_nudge":
        if (event.outcome === "retry_exhausted") degradations.push("Exhausted quality-floor retries");
        else retries.push("Retried below the evidence floor");
        break;
      case "critique_retry_denied":
        degradations.push(`Critique retry denied: ${phrase(event.outcome)}`);
        break;
      case "critique_retry":
        if (event.outcome && critiqueRetryProblems.has(event.outcome)) {
          degradations.push(`Critique repair failed: ${phrase(event.outcome)}`);
        } else {
          retries.push(`Critique retry: ${phrase(event.outcome)}`);
        }
        break;
      case "context_headroom":
        if (event.outcome && headroomProblems[event.outcome]) {
          degradations.push(headroomProblems[event.outcome]);
        } else if (event.outcome === "over_budget") {
          degradations.push("Still over the context budget after compaction");
        }
        break;
      case "structured_completion":
        if (event.structured_outcome === "accepted") {
          recoveredStructured.push(...pendingStructured);
          pendingStructured = [];
        } else if (event.structured_outcome) {
          pendingStructured.push(phrase(event.structured_outcome));
        }
        break;
      case "critique":
        if (event.outcome === "published_rejected") {
          degradations.push(`Critique rejected the published analysis${issueSuffix(event.issue_count)}`);
        } else if (event.outcome === "published_warning") {
          degradations.push(`Published over critique warnings${issueSuffix(event.issue_count)}`);
        } else if (event.outcome === "objected") {
          retries.push(`Critique objected${issueSuffix(event.issue_count)}`);
        }
        break;
      case "finalize":
        if (event.outcome && finalizeProblems.has(event.outcome)) {
          degradations.push(`Finalize ${phrase(event.outcome)}`);
        }
        break;
      case "finalize_recovery":
        degradations.push(`Finalize recovered: ${phrase(event.outcome)}`);
        break;
      case "cache_persistence":
        if (event.outcome && cacheProblems.has(event.outcome)) {
          degradations.push(`Analysis not cached: ${phrase(event.outcome)}`);
        }
        break;
      case "publication":
        if (event.outcome === "unavailable") degradations.push("Published as unavailable");
        break;
      case "context_compaction":
        // The backend records compaction before checking whether it fit, so this
        // stays neutral and the over_budget rule reports an actual shortfall.
        retries.push("Conversation compacted");
        break;
      default:
        break;
    }
  }

  // The structured path walks response_format -> forced_function -> plain
  // fallback, so a failure a later attempt recovers is a fallback rather than a
  // defect. Only failures still outstanding at the end of the trace degrade it.
  if (recoveredStructured.length > 0) {
    retries.push(`Recovered from structured output ${dedupe(recoveredStructured).join(", ")}`);
  }
  if (pendingStructured.length > 0) {
    degradations.push(`Never accepted structured output ${dedupe(pendingStructured).join(", ")}`);
  }

  const severity: AnalysisHealthSeverity = failures.length
    ? "failed"
    : degradations.length
      ? "degraded"
      : retries.length
        ? "retried"
        : "healthy";
  const reasons = dedupe([...failures, ...degradations, ...retries]);

  return { severity, reasons, modelRequests, toolCalls, toolErrors, evidenceBytes };
}

function issueSuffix(count?: number): string {
  if (!count) return "";
  return `: ${count} issue${count === 1 ? "" : "s"}`;
}

function dedupe(values: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const value of values) {
    if (seen.has(value)) continue;
    seen.add(value);
    out.push(value);
  }
  return out;
}

/** Reports whether a severity belongs in the default problem-first view. */
export function needsAttention(severity: AnalysisHealthSeverity): boolean {
  return severity === "failed" || severity === "degraded";
}

export interface AnalysisHealthEntry<T> {
  item: T;
  verdict: AnalysisHealthVerdict;
}

/**
 * Orders analyses most urgent first, then most recent first within a severity.
 * Traces without a parseable timestamp sort last.
 */
export function rankAnalysisHealth<T>(
  items: T[],
  traceOf: (item: T) => AnalysisTrace,
): AnalysisHealthEntry<T>[] {
  const rank = new Map(analysisHealthSeverities.map((severity, index) => [severity, index]));
  return items
    .map((item) => ({ item, verdict: analysisHealthVerdict(traceOf(item)) }))
    .sort((a, b) => {
      const bySeverity =
        (rank.get(a.verdict.severity) ?? 0) - (rank.get(b.verdict.severity) ?? 0);
      if (bySeverity !== 0) return bySeverity;
      return startedAtMs(traceOf(b.item)) - startedAtMs(traceOf(a.item));
    });
}

function startedAtMs(trace: AnalysisTrace): number {
  const parsed = Date.parse(trace.recorded_at ?? trace.started_at);
  return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed;
}

export type AnalysisHealthCounts = Record<AnalysisHealthSeverity, number>;

/** Counts analyses per severity across a ranked ledger. */
export function analysisHealthCounts<T>(entries: AnalysisHealthEntry<T>[]): AnalysisHealthCounts {
  const counts: AnalysisHealthCounts = { failed: 0, degraded: 0, retried: 0, healthy: 0 };
  for (const entry of entries) counts[entry.verdict.severity]++;
  return counts;
}
