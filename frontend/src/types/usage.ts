export type AIUsageFeature =
  | "failure_analysis" | "pattern_analysis" | "analysis_chat" | "issue_draft"
  | "fix_preview" | "fix_critique" | "pr_template" | "source_investigation";

export interface AIUsageTotals {
  operations: number; cache_hits: number; suppressed_operations?: number; cooldown_retries?: number; failures: number;
  external_unmetered_operations: number; model_gateway_excluded_operations?: number; model_requests: number;
  reported_requests: number; priced_reported_requests?: number;
  cache_write_reported_requests?: number; cache_write_priced_requests?: number;
  cache_write_unreported_requests?: number; invalid_usage_requests?: number; unreported_requests: number;
  input_tokens: number; cached_input_tokens: number; cache_write_input_tokens?: number; output_tokens: number;
  reasoning_tokens: number; estimated_cost_nanos: string;
}
export interface AIUsageOperation {
  id: string; logical_id?: string; origin: string; feature: AIUsageFeature;
  started_at: string; completed_at: string; outcome: string; currency?: string;
  model?: string; model_fingerprint?: string; mixed_models?: boolean; usage_source?: string;
  model_requests?: number; reported_requests?: number; unreported_requests?: number;
  cache_write_reported_requests?: number; cache_write_priced_requests?: number; cache_write_unreported_requests?: number;
  input_tokens?: number; cached_input_tokens?: number; cache_write_input_tokens?: number; output_tokens?: number;
  reasoning_tokens?: number; estimated_cost_nanos?: number; external_unmetered?: boolean;
  model_gateway_excluded?: boolean; coverage_counts_known?: boolean;
  cooldown_retry?: boolean;
}
export interface AIUsageReport {
  version: number; generated_at: string; range: { start: string; end: string };
  currency?: string; mixed_currency?: boolean; mixed_pricing?: boolean;
  coverage: {
    status: "complete" | "partial" | "unavailable"; states?: string[];
    model_requests: number; reported_requests: number; priced_reported_requests?: number;
    cache_write_reported_requests?: number; cache_write_priced_requests?: number; cache_write_unreported_requests?: number;
    invalid_usage_requests?: number; unreported_requests: number; external_unmetered_operations: number;
    model_gateway_excluded_operations?: number; pricing_added_after_requests?: number; legacy_coverage_unknown?: boolean;
  };
  totals: AIUsageTotals;
  daily: Array<{ date: string; totals: AIUsageTotals }>;
  features: Array<{ feature: AIUsageFeature; totals: AIUsageTotals }>;
  models?: Array<{ model: string; totals: AIUsageTotals }>;
  model_coverage?: "complete" | "partial" | "unavailable" | "unavailable_for_feature_filter";
  recent_operations: AIUsageOperation[];
  selected_model?: string; pricing_rule?: string; pricing_configured: boolean; range_priced: boolean; pricing_coverage: "complete" | "partial" | "unavailable" | "unknown";
}
