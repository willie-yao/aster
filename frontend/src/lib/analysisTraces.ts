import type { AnalysisTrace, AnalysisTraceEvent } from "../types/traces";

export const analysisTraceFilterKeys = [
  "job_id",
  "build_id",
  "test_name",
  "response_id",
] as const;

export type AnalysisTraceFilterKey = (typeof analysisTraceFilterKeys)[number];

export const analysisTraceFilterLabels: Record<AnalysisTraceFilterKey, string> = {
  job_id: "Job ID",
  build_id: "Build ID",
  test_name: "Test name",
  response_id: "Response ID",
};

export type TraceTone = "success" | "warning" | "error" | "neutral";

export function analysisTraceActiveFilterCount(params: URLSearchParams): number {
  return analysisTraceFilterKeys.filter((key) => Boolean(params.get(key)?.trim())).length;
}

export function traceTone(outcome?: string): TraceTone {
  const value = outcome?.toLowerCase() ?? "";
  if (/(success|succeeded|passed|completed|accepted|revised)/.test(value)) return "success";
  if (/(retry|objected|truncated|denied|over_budget|uncached)/.test(value)) return "warning";
  if (/(error|failed|cancelled|unavailable|rejected|exhausted|empty)/.test(value)) return "error";
  return "neutral";
}

export function traceStatusLabel(value?: string): string {
  const label = value?.replaceAll("_", " ").replaceAll("-", " ").trim() ?? "";
  if (!label) return "Not reported";
  const words = label.split(/\s+/u).map((word) => {
    if (word === "ai") return "AI";
    if (word === "api") return "API";
    if (word === "http") return "HTTP";
    return word;
  });
  const normalized = words.join(" ");
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

export function formatTraceDuration(ms?: number): string {
  if (ms === undefined) return "";
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`;
}

export function analysisTraceEventDetails(event: AnalysisTraceEvent): string[] {
  const details: string[] = [];
  if (event.response_id) details.push(`response ${event.response_id}`);
  if (event.tool) details.push(event.tool);
  if (event.status) details.push(`status ${event.status}`);
  if (event.finish_reason) details.push(`finish ${event.finish_reason}`);
  if (event.duration_ms !== undefined) details.push(`request ${formatTraceDuration(event.duration_ms)}`);
  if (event.attempts && event.attempts > 1) details.push(`${event.attempts} attempts`);
  if (event.http_status) details.push(`HTTP ${event.http_status}`);
  if (event.input_tokens || event.output_tokens) {
    details.push(`${event.input_tokens ?? 0} in / ${event.output_tokens ?? 0} out`);
  }
  if (event.message_count) details.push(`${event.message_count} messages`);
  if (event.tool_call_count) details.push(`${event.tool_call_count} tool calls`);
  if (event.bytes) details.push(`${event.bytes.toLocaleString()} bytes`);
  if (event.elided) details.push(`${event.elided} elided`);
  if (event.retry) details.push(`retry ${event.retry}`);
  if (event.issue_count) {
    details.push(`${event.issue_count} issue${event.issue_count === 1 ? "" : "s"}`);
  }
  if (event.critique_rules?.length) details.push(`rules ${event.critique_rules.join(", ")}`);
  if (event.cache_rejection_reason) details.push(`not cached: ${event.cache_rejection_reason}`);
  if (event.validation_code) details.push(event.validation_code);
  if (event.error_code) details.push(event.error_code);
  return details;
}

export function analysisTraceResponseIDs(trace: AnalysisTrace): string[] {
  const seen = new Set<string>();
  const ids: string[] = [];
  for (const event of trace.events) {
    const value = event.response_id?.trim();
    if (!value || seen.has(value)) continue;
    seen.add(value);
    ids.push(value);
  }
  return ids;
}
