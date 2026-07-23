// Types matching the backend JSON output.

export interface RunSummary {
  build_id: string;
  passed: boolean;
  result?: string;
  timestamp: string;
  duration_seconds?: number;
  tests_total?: number;
  tests_passed?: number;
  tests_failed?: number;
  tests_skipped?: number;
}

export interface JobSummary {
  name: string;
  job_id: string;
  job_type: "periodic" | "presubmit";
  repo: string;
  tab_name: string;
  category: string;
  branch: string;
  description: string;
  minimum_interval: string;
  timeout: string;
  config_file: string;
  overall_status: "PASSING" | "FAILING" | "FLAKY";
  last_run: RunSummary | null;
  recent_runs: RunSummary[];
  // Fraction of passing runs over the most recent runs.
  pass_rate_recent: number;
}

export interface Dashboard {
  generated_at: string;
  jobs: JobSummary[];
}

export interface TestCase {
  name: string;
  status: "passed" | "failed" | "skipped";
  duration_seconds: number;
  failure_message?: string;
  failure_body?: string;
  failure_location?: string;
  failure_location_url?: string;
  junit_file?: string;
  cluster_artifacts?: ClusterArtifacts;
  ai_summary?: AISummary;
  ai_analysis?: AIAnalysis;
}

export interface AISummary {
  generated_at: string;
  summary: string;
  is_transient: boolean;
}

export interface AIAnalysis {
  generated_at: string;
  model: string;
  root_cause: string;
  severity: string;
  suggested_fix: string;
  relevant_files?: string[];
  // Verified GitHub links for cited source files keyed by cleaned path. When
  // present, this map is authoritative and absent files stay unlinked.
  file_links?: Record<string, string>;
  mode?: string;
  // Optional per-analysis telemetry emitted by the backend. Cached analyses may
  // omit metrics or record zero when a metric is unavailable.
  tool_calls?: number;
  tool_failures?: number;
  model_requests?: number;
  model_failures?: number;
  context_bytes?: number;
  context_truncations?: number;
  gcs_bytes?: number;
  elapsed_ms?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_hit?: boolean;
  budget_exhausted?: boolean;
}

export interface ClusterArtifacts {
  cluster_name: string;
  provider_activity_log?: string;
  machines?: MachineArtifacts[];
  pod_log_dirs?: Record<string, string>;
  bootstrap_resources_url?: string;
}

export interface MachineArtifacts {
  name: string;
  logs: Record<string, string>;
}

export interface BuildResult {
  build_id: string;
  job_name: string;
  pull_number?: string;
  started: string;
  finished: string;
  passed: boolean;
  result: string;
  duration_seconds: number;
  commit: string;
  repo_version?: string;
  prow_url: string;
  web_url: string;
  build_log_url: string;
  junit_urls?: string[];
  junit_complete?: boolean;
  test_cases: TestCase[];
  tests_total: number;
  tests_passed: number;
  tests_failed: number;
  tests_skipped: number;
  controller_log_urls?: Record<string, string>;
}

export interface TestFlakiness {
  test_name: string;
  job_name: string;
  job_id: string;
  total_runs: number;
  failures: number;
  passes: number;
  flip_rate: number;
  fail_rate: number;
  consecutive_failures: number;
  classification: "persistent" | "flaky" | "one-off";
  last_failure?: {
    build_id: string;
    timestamp: string;
    failure_message: string;
    error_hash: string;
  };
  first_failed_at?: string;
  error_patterns?: {
    normalized_message: string;
    error_hash: string;
    count: number;
    example_message: string;
  }[];
}

export interface FlakinessReport {
  generated_at: string;
  most_flaky: TestFlakiness[];
  persistent_failures: TestFlakiness[];
  recently_broken: TestFlakiness[];
  recurring_patterns?: PatternAnalysis[];
}

export interface PatternAnalysis {
  id?: string;
  subject: string;
  job_id?: string;
  generated_at: string;
  builds_analyzed: number;
  systemic: boolean;
  confidence: "high" | "medium" | "low";
  shared_root_cause?: string;
  shared_builds?: string[];
  suggested_fix?: string;
  summary: string;
}

export interface JobDetail {
  name: string;
  job_id: string;
  job_type: "periodic" | "presubmit";
  repo: string;
  runs: BuildResult[];
  pattern_analyses?: PatternAnalysis[];
}

export interface SearchEntry {
  kind: "job" | "test";
  test_name: string;
  job_name: string;
  job_id: string;
  job_type: "periodic" | "presubmit";
  repo: string;
  tab_name: string;
  branch: string;
  category: string;
  status: string;
  fail_rate: number;
}

export interface SearchIndex {
  generated_at: string;
  entries: SearchEntry[];
}

export interface ResolvedEntry {
  resolved_at: string;
  resolved_by: string;
  note?: string;
  watermark: string;
  subject?: string;
}

export interface ResolvedState {
  resolved: Record<string, ResolvedEntry>;
}

export interface RemediationObservation {
  build_id: string;
  job_name: string;
  job_type: "periodic" | "presubmit";
  result: string;
  outcome: string;
  reason?: string;
  prow_url?: string;
  started_at?: string;
  completed_at?: string;
}

export interface RemediationAttempt {
  number: number;
  pr_number: number;
  url: string;
  target_repo: string;
  head_sha?: string;
  merge_sha?: string;
  status: string;
  pr_state?: string;
  outcome?: string;
  outcome_reason?: string;
  observations?: RemediationObservation[];
}

export interface RemediationEntry {
  id: string;
  subject: string;
  job_id: string;
  job_name: string;
  job_type: "periodic" | "presubmit";
  issue?: { number: number; url: string; state?: string };
  attempt?: RemediationAttempt;
  updated_at: string;
}

export interface RemediationState {
  remediations: Record<string, RemediationEntry>;
}
