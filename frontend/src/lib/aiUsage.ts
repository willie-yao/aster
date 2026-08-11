import type { AIUsageDaily, AIUsageFeature, AIUsageTotals } from "../types/usage";
export const featureLabels: Record<AIUsageFeature, string> = {
  failure_analysis: "Failure analysis", pattern_analysis: "Pattern analysis",
  analysis_chat: "Analysis chat", issue_draft: "Issue drafts", fix_preview: "Fix PR preview",
  fix_critique: "Fix critique", pr_template: "PR templates", source_investigation: "Source investigation",
};
export function formatTokens(value: number): string {
  return new Intl.NumberFormat("en-US", { notation: value >= 100000 ? "compact" : "standard", maximumFractionDigits: 1 }).format(value);
}
export function formatCost(nanos: string, currency?: string): string {
  if (!currency) return "Not priced";
  const value = Number(nanos) / 1_000_000_000;
  return new Intl.NumberFormat("en-US", { style: "currency", currency, minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}
export function totalTokens(t: AIUsageTotals): number { return t.input_tokens + t.output_tokens; }
export function uncachedInputTokens(t: AIUsageTotals): number {
  return Math.max(0, t.input_tokens - t.cached_input_tokens - (t.cache_write_input_tokens ?? 0));
}
export function formatExactTokens(value: number): string { return value.toLocaleString("en-US"); }
export function formatExactCost(nanos: string | undefined, currency?: string): string {
  if (!nanos || !currency) return "Unavailable";
  try {
    const cents = (BigInt(nanos) + 5_000_000n) / 10_000_000n;
    const whole = cents / 100n; const fraction = (cents % 100n).toString().padStart(2, "0");
    return `${currency} ${whole.toLocaleString("en-US")}.${fraction}`;
  } catch { return "Unavailable"; }
}
export function formatChartCost(value: number, currency?: string): string {
  if (!Number.isFinite(value)) return "Unavailable";
  if (!currency) return value.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  return new Intl.NumberFormat("en-US", {
    style: "currency", currency, minimumFractionDigits: 2, maximumFractionDigits: 2,
  }).format(value);
}
export function chartTickValues(max: number, targetIntervals = 4): number[] {
  if (!Number.isFinite(max) || max <= 0 || targetIntervals < 1) return [];
  const roughStep = max / targetIntervals;
  const magnitude = 10 ** Math.floor(Math.log10(roughStep));
  const normalized = roughStep / magnitude;
  const niceStep = (normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 2.5 ? 2.5 : normalized <= 5 ? 5 : 10) * magnitude;
  const axisMax = Math.ceil(max / niceStep) * niceStep;
  return Array.from({ length: Math.round(axisMax / niceStep) + 1 }, (_, index) => index * niceStep);
}
export function chartDateTickIndexes(count: number, maxTicks = 6): number[] {
  if (count <= 0 || maxTicks <= 0) return [];
  if (count <= maxTicks) return Array.from({ length: count }, (_, index) => index);
  if (maxTicks === 1) return [count - 1];
  return Array.from(new Set(Array.from({ length: maxTicks }, (_, index) => Math.round(index * (count - 1) / (maxTicks - 1)))));
}
export function nearestChartDataIndex(target: number, available: number[]): number | null {
  if (available.length === 0) return null;
  return available.reduce((nearest, index) => Math.abs(index - target) < Math.abs(nearest - target) ? index : nearest, available[0]);
}
export function chartViewBoxLayout(viewportWidth: number, viewportHeight: number, viewBoxWidth: number, viewBoxHeight: number): { scale: number; offsetX: number; offsetY: number } {
  if (viewportWidth <= 0 || viewportHeight <= 0 || viewBoxWidth <= 0 || viewBoxHeight <= 0) {
    return { scale: 1, offsetX: 0, offsetY: 0 };
  }
  const scale = Math.min(viewportWidth / viewBoxWidth, viewportHeight / viewBoxHeight);
  return {
    scale,
    offsetX: (viewportWidth - viewBoxWidth * scale) / 2,
    offsetY: (viewportHeight - viewBoxHeight * scale) / 2,
  };
}
export function chartViewBoxPoint(viewportX: number, viewportY: number, layout: ReturnType<typeof chartViewBoxLayout>): { x: number; y: number } {
  return { x: (viewportX - layout.offsetX) / layout.scale, y: (viewportY - layout.offsetY) / layout.scale };
}
export function chartViewportX(viewBoxX: number, layout: ReturnType<typeof chartViewBoxLayout>): number {
  return layout.offsetX + viewBoxX * layout.scale;
}
export function chartScale(max: number, hasData: boolean): { ticks: number[]; max: number } {
  if (!hasData) return { ticks: [], max: 0 };
  if (!Number.isFinite(max) || max <= 0) return { ticks: [0, 0.01], max: 0.01 };
  const maxCents = Math.max(1, Math.ceil(max * 100));
  const initialTicks = chartTickValues(maxCents);
  const stepCents = Math.max(1, Math.ceil((initialTicks[1] ?? maxCents) - initialTicks[0]));
  const axisMaxCents = Math.ceil(maxCents / stepCents) * stepCents;
  const ticks = Array.from({ length: Math.round(axisMaxCents / stepCents) + 1 }, (_, index) => index * stepCents / 100);
  return { ticks, max: axisMaxCents / 100 };
}
export function chartCurrencyPolicy(recordedCurrency?: string, currentCurrency?: string, mixedRecorded = false): { showRecorded: boolean; showCurrent: boolean; note?: string } {
  if (mixedRecorded) {
    return { showRecorded: false, showCurrent: true, note: "Recorded series omitted because recorded estimates contain multiple currencies." };
  }
  if (recordedCurrency && currentCurrency && recordedCurrency !== currentCurrency) {
    return { showRecorded: true, showCurrent: false, note: `Current-rate series omitted because current rates use ${currentCurrency} while recorded estimates use ${recordedCurrency}.` };
  }
  return { showRecorded: true, showCurrent: true };
}
export function chartSeriesDescription(hasRecorded: boolean, hasCurrent: boolean): string {
  const series = [];
  if (hasRecorded) series.push("Solid blue shows recorded estimates.");
  if (hasCurrent) series.push("Dashed amber shows current-rate estimates.");
  return `${series.join(" ")} Hover over the chart or focus it and use the left and right arrow keys to inspect dates. Exact daily values are listed in the table below.`.trim();
}
export function usageQuery(start: string, end: string, feature?: AIUsageFeature): string {
  const query = new URLSearchParams({ start, end }); if (feature) query.append("feature", feature); return query.toString();
}

export function formatCoverage(covered: number, total: number): string {
  const percent = total > 0 ? Math.round((covered / total) * 100) : 0;
  return `${covered.toLocaleString()} of ${total.toLocaleString()} (${percent}%)`;
}

export function pricedRequestCoverageNote(
  pricedRequests: number | undefined,
  reportedRequests: number,
  pricingCoverage: "complete" | "partial" | "unavailable" | "unknown",
): string {
  if (pricedRequests === undefined) {
    return "Priced-request coverage is unavailable for legacy records";
  }
  if (pricingCoverage === "unavailable") {
    return "Pricing coverage is unavailable";
  }
  if (pricingCoverage === "unknown") {
    return "Pricing coverage is unavailable for some legacy records";
  }
  return `${formatCoverage(pricedRequests, reportedRequests)} reported requests priced`;
}

export interface AIUsageFilterValues {
  start: string;
  end: string;
  feature: AIUsageFeature | "";
}

export function defaultAIUsageFilters(now = new Date()): AIUsageFilterValues {
  const end = new Date(now);
  const start = new Date(now);
  start.setUTCDate(start.getUTCDate() - 29);
  return {
    start: start.toISOString().slice(0, 10),
    end: end.toISOString().slice(0, 10),
    feature: "",
  };
}

export function aiUsageFiltersFromParams(
  params: URLSearchParams,
  defaults: AIUsageFilterValues,
): AIUsageFilterValues {
  const feature = params.get("feature") ?? "";
  return {
    start: params.get("start") || defaults.start,
    end: params.get("end") || defaults.end,
    feature: Object.hasOwn(featureLabels, feature) ? feature as AIUsageFeature : "",
  };
}

export function aiUsageFilterParams(values: AIUsageFilterValues): URLSearchParams {
  return new URLSearchParams(usageQuery(values.start, values.end, values.feature || undefined));
}

export function aiUsageFiltersAreCustom(params: URLSearchParams): boolean {
  return ["start", "end", "feature"].some((key) => params.has(key));
}

export function aiUsageFilterSummary(values: AIUsageFilterValues): string {
  return `${values.start} to ${values.end} · ${values.feature ? featureLabels[values.feature] : "All features"}`;
}

export function coverageStateLabel(value: string): string {
  const labels: Record<string, string> = {
    fully_priced_provider_reported: "Fully priced provider usage",
    partial_token_usage: "Partial token usage",
    cache_write_unreported: "Missing cache-write usage",
    cache_write_pricing_missing: "Missing cache-write rate",
    external_unmetered: "External unmetered operations",
    model_gateway_excluded: "Model gateway excluded operations",
    pricing_added_after_operation: "Pricing added after operation",
    pricing_unavailable: "Pricing unavailable",
    legacy_coverage_unknown: "Legacy coverage unknown",
    aggregate_overflow: "Aggregate overflow blocked",
    no_provider_usage: "No provider usage",
    no_usage_records: "No usage records",
  };
  return labels[value] ?? value.replaceAll("_", " ");
}

export function featureTokenPercentage(featureTokens: number, selectedTokens: number): number {
  if (selectedTokens <= 0 || featureTokens <= 0) return 0;
  return Math.min(100, featureTokens / selectedTokens * 100);
}

export function normalizeAIUsageDay(day: AIUsageDaily): AIUsageDaily {
  return {
    ...day,
    features: day.features ?? [],
    coverage: day.coverage ?? {
      status: "unavailable",
      states: ["legacy_coverage_unknown"],
      model_requests: day.totals.model_requests,
      reported_requests: day.totals.reported_requests,
      unreported_requests: day.totals.unreported_requests,
      external_unmetered_operations: day.totals.external_unmetered_operations,
    },
    has_usage: day.has_usage ?? day.totals.operations > 0,
    current_partial_utc: day.current_partial_utc ?? false,
    recorded_cost_status: day.recorded_cost_status ?? "unknown",
    current_rate_status: day.current_rate_status ?? "unavailable",
  };
}

export function formatRecordedUsageEstimate({
  status,
  mixedCurrency,
  nanos,
  currency,
}: {
  status: "available" | "partial" | "unavailable" | "unknown" | "mixed_currency";
  mixedCurrency?: boolean;
  nanos: string | undefined;
  currency?: string;
}): string {
  if (mixedCurrency || status === "mixed_currency") return "Mixed currencies";
  if (status === "unavailable") return "Not priced";
  if (status === "unknown") return "Unknown";
  return formatExactCost(nanos, currency);
}

export function formatCurrentRateReprice({
  status,
  nanos,
  currency,
}: {
  status: "available" | "partial" | "unavailable";
  nanos: string | undefined;
  currency?: string;
}): string {
  if (status === "unavailable") return "Unavailable";
  return formatExactCost(nanos, currency);
}
