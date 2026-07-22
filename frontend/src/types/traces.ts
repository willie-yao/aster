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
  error_code?: string;
}

export interface AnalysisTrace {
  backend: string;
  job_id: string;
  build_id: string;
  test_name: string;
  api_mode: string;
  started_at: string;
  elapsed_ms: number;
  outcome: string;
  error_code?: string;
  truncated?: boolean;
  events: AnalysisTraceEvent[];
}

export interface AnalysisTraceFile {
  version: number;
  generated_at: string;
  dropped_traces?: number;
  traces: AnalysisTrace[];
}
