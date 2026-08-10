import type { AIUsageFeature, AIUsageTotals } from "../types/usage";
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
export function chartScale(max: number, hasData: boolean): { ticks: number[]; max: number } {
  if (!hasData) return { ticks: [], max: 0 };
  if (!Number.isFinite(max) || max <= 0) return { ticks: [0], max: 1 };
  const ticks = chartTickValues(max);
  return { ticks, max: ticks.at(-1) ?? max };
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
  if (pricingCoverage === "unknown") {
    return "Pricing coverage is unavailable for some legacy records";
  }
  return `${formatCoverage(pricedRequests, reportedRequests)} reported requests priced`;
}
