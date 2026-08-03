export type AIUsageFeature =
  | "failure_analysis" | "pattern_analysis" | "analysis_chat" | "issue_draft"
  | "fix_preview" | "fix_critique" | "pr_template" | "source_investigation";

export interface AIUsageTotals {
  operations: number; cache_hits: number; failures: number;
  external_unmetered_operations: number; model_requests: number;
  reported_requests: number; unreported_requests: number;
  input_tokens: number; cached_input_tokens: number; output_tokens: number;
  reasoning_tokens: number; estimated_cost_nanos: string;
}
export interface AIUsageOperation {
  id: string; logical_id?: string; origin: string; feature: AIUsageFeature;
  started_at: string; completed_at: string; outcome: string; currency?: string;
  model_requests?: number; reported_requests?: number; unreported_requests?: number;
  input_tokens?: number; cached_input_tokens?: number; output_tokens?: number;
  reasoning_tokens?: number; estimated_cost_nanos?: number; external_unmetered?: boolean;
}
export interface AIUsageReport {
  version: number; generated_at: string; range: { start: string; end: string };
  currency?: string; mixed_currency?: boolean; mixed_pricing?: boolean;
  coverage: { status: "complete" | "partial" | "unavailable"; model_requests: number; reported_requests: number; unreported_requests: number; external_unmetered_operations: number };
  totals: AIUsageTotals;
  daily: Array<{ date: string; totals: AIUsageTotals }>;
  features: Array<{ feature: AIUsageFeature; totals: AIUsageTotals }>;
  recent_operations: AIUsageOperation[];
}
