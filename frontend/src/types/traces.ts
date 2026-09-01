export interface AnalysisTraceEvent {
  sequence: number;
  elapsed_ms: number;
  kind: string;
  outcome?: string;
  response_id?: string;
  status?: string;
  finish_reason?: string;
  tool?: string;
  duration_ms?: number;
  attempts?: number;
  http_status?: number;
  input_tokens?: number;
  output_tokens?: number;
  message_count?: number;
  tool_call_count?: number;
  bytes?: number;
  elided?: number;
  retry?: number;
  issue_count?: number;
  critique_rules?: string[];
  cache_rejection_reason?: string;
  structured_phase?: string;
  structured_attempt?: string;
  structured_outcome?: string;
  error_code?: string;
  validation_code?: string;
}

export interface AnalysisTrace {
  job_id: string;
  build_id: string;
  test_name: string;
  api_mode: string;
  model?: string;
  reasoning_effort?: string;
  started_at: string;
  recorded_at?: string;
  elapsed_ms: number;
  outcome: string;
  error_code?: string;
  truncated?: boolean;
  events: AnalysisTraceEvent[];
}

export interface AnalysisTraceEngine {
  version: string;
  commit: string;
  image_tag: string;
}

export interface AnalysisTraceFile {
  version: number;
  generated_at: string;
  retained_since?: string;
  dropped_traces?: number;
  engine?: AnalysisTraceEngine;
  traces: AnalysisTrace[];
}
