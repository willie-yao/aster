import type { AIUsageFeature, AIUsageTotals } from "../types/usage";
export const featureLabels: Record<AIUsageFeature, string> = {
  failure_analysis: "Failure analysis", pattern_analysis: "Pattern analysis",
  analysis_chat: "Analysis chat", issue_draft: "Issue drafts", fix_preview: "Fix previews",
  fix_critique: "Fix critique", pr_template: "PR templates", source_investigation: "Source investigation",
};
export function formatTokens(value: number): string {
  return new Intl.NumberFormat("en-US", { notation: value >= 100000 ? "compact" : "standard", maximumFractionDigits: 1 }).format(value);
}
export function formatCost(nanos: string, currency?: string): string {
  if (!currency) return "Not priced";
  const value = Number(nanos) / 1_000_000_000;
  return new Intl.NumberFormat("en-US", { style: "currency", currency, minimumFractionDigits: value < 1 ? 4 : 2, maximumFractionDigits: value < 1 ? 6 : 2 }).format(value);
}
export function totalTokens(t: AIUsageTotals): number { return t.input_tokens + t.output_tokens; }
export function usageQuery(start: string, end: string, feature?: AIUsageFeature): string {
  const query = new URLSearchParams({ start, end }); if (feature) query.append("feature", feature); return query.toString();
}
