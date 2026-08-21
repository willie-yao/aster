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

export type JobCurrentStatus = "UNKNOWN" | "RUNNING" | "PASSING" | "FAILING";

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
  current_status: JobCurrentStatus;
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
  suite_name?: string;
  class_name?: string;
  source?: "build";
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

export interface AnalysisCauseLocation {
  // Owning "owner/repo".
  repository: string;
  // True when the cause lives in a dependency rather than this project's repo.
  external?: boolean;
  // Path hints inside repository. Paths in a dependency are never read at a
  // pinned revision, so they are hints and never appear in file_links.
  files?: string[];
}

export interface AIAnalysis {
  generated_at: string;
  model: string;
  root_cause: string;
  severity: string;
  suggested_fix: string;
  relevant_files?: string[];
  disposition?: "preliminary" | "grounded";
  disposition_warnings?: string[];
  // Verified GitHub links for cited source files keyed by cleaned path. When
  // present, this map is authoritative and absent files stay unlinked.
  file_links?: Record<string, string>;
  cause_location?: AnalysisCauseLocation;
  mode?: string;
  critique_passed?: boolean;
  critique_version?: number;
  // Optional per-analysis telemetry emitted by the backend. Cached analyses may
  // omit metrics or record zero when a metric is unavailable.
  tool_calls?: number;
  tool_failures?: number;
  model_requests?: number;
  model_failures?: number;
  context_bytes?: number;
  context_truncations?: number;
  gcs_bytes?: number;
  evidence_plan_covered?: boolean;
  gcs_floor_retry_exhausted?: boolean;
  elapsed_ms?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_hit?: boolean;
  same_failure_reuse?: boolean;
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
  junit_truncated?: boolean;
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

export type PatternRefreshState =
  "current" | "retained" | "failed" | "not_applicable" | "unavailable";

export interface PatternRefreshStatus {
  state: PatternRefreshState;
  last_successful_at?: string;
  attempts?: number;
  repairs?: number;
  failure_category?: string;
  evidence_available: boolean;
}

export interface PatternRefreshReport {
  current: number;
  retained: number;
  failed: number;
  unavailable: number;
  not_applicable: number;
  jobs?: Record<string, PatternRefreshStatus>;
}

export interface BuildFailureSummary {
  job_id: string;
  job_name: string;
  build_id: string;
  started_at: string;
  result: string;
  analysis_state: "succeeded" | "unavailable";
  summary?: string;
  severity?: string;
  is_transient: boolean;
  provenance?: "cache";
  build_log_url?: string;
  job_detail_url: string;
}

export interface LowPassRateEntry extends TestFlakiness {
  // Number of runs the pass rate was measured over. May be narrower than the
  // window behind fail_rate when attention.low_pass_rate.recent_runs is set.
  window_runs: number;
  pass_rate: number;
}

export interface FlakinessReport {
  generated_at: string;
  most_flaky: TestFlakiness[];
  persistent_failures: TestFlakiness[];
  recently_broken: TestFlakiness[];
  // Tests selected by the optional attention.low_pass_rate rule. Empty when
  // the rule is not configured. Selection does not change classification.
  low_pass_rate?: LowPassRateEntry[];
  build_failures: BuildFailureSummary[];
  recurring_patterns?: PatternAnalysis[];
  pattern_refresh?: PatternRefreshReport;
}

export interface PatternCausalGroup {
  id?: string;
  content_hash?: string;
  builds: string[];
  root_cause: string;
  confidence: "high" | "medium" | "low";
  // Durable identity of this cause across build windows. Written by the engine
  // for its own recurrence memory; the UI does not render it.
  signature?: string;
  cause_location?: AnalysisCauseLocation;
  // The action this cause's own member analyses reported. A suggestion, not a
  // verified target: acting on a cause still goes through the remediation
  // investigation, which verifies deterministically.
  remediation?: PatternCausalGroupRemediation;
}

export interface PatternCausalGroupRemediation {
  suggested_fix: string;
  build_id: string;
}

export type PatternRecurrence =
  "shared_cause" | "mixed_causes" | "unrelated" | "insufficient_evidence";

export type PatternRemediationInvestigationState =
  | "not_investigated"
  | "queued"
  | "investigating"
  | "verifying"
  | "actionable"
  | "already_fixed"
  | "external_dependency"
  | "environment_or_infrastructure"
  | "mitigation_only"
  | "insufficient_evidence"
  | "failed"
  | "stale"
  | "evidence_expired";

export interface PatternRemediationTargetSummary {
  kind: string;
  repository: string;
  revision: string;
  path: string;
  symbol?: string;
  required_call?: string;
  job?: string;
  container?: string;
  name?: string;
  value?: string;
}

export interface PatternRemediationInvestigationSummary {
  causal_group_id: string;
  causal_group_hash: string;
  state: PatternRemediationInvestigationState;
  reason?: string;
  target?: PatternRemediationTargetSummary;
  completed_at?: string;
}

export interface PatternAnalysis {
  id?: string;
  content_hash?: string;
  subject: string;
  job_id?: string;
  generated_at: string;
  builds_analyzed: number;
  recurrence_classification?: PatternRecurrence;
  causal_groups?: PatternCausalGroup[];
  unclassified_builds?: string[];
  systemic: boolean;
  confidence: "high" | "medium" | "low";
  shared_root_cause?: string;
  shared_builds?: string[];
  suggested_fix?: string;
  remediation_targets?: RemediationTarget[];
  relevant_files?: string[];
  file_links?: Record<string, string>;
  source_ref?: string;
  remediation_verification?: PatternRemediationVerification;
  remediation_investigations?: PatternRemediationInvestigationSummary[];
  lifecycle?: PatternLifecycle;
  summary: string;
}

export interface PatternRemediationVerification {
  state: "unresolved" | "already_present" | "inconclusive";
  reason: string;
  repository?: string;
  revision?: string;
  failure_state?: "unresolved" | "already_present" | "inconclusive";
  failure_builds?: string[];
  passing_builds?: string[];
}

export interface PatternLifecycle {
  state: "active" | "recovered" | "observing" | "verified_fixed";
  reason: string;
  source_revision?: string;
  passing_builds?: string[];
  recovery_streak?: number;
  recovery_builds?: string[];
}

export interface RemediationTarget {
  intent:
    | "add_symbol"
    | "modify_symbol"
    | "set_configuration"
    | "remove_configuration"
    | "set_job_environment"
    | "investigate";
  symbol?: string;
  required_call?: string;
  path?: string;
  value?: string;
  repository?: string;
  revision?: string;
  job?: string;
  container?: string;
  name?: string;
}

export interface JobDetail {
  name: string;
  job_id: string;
  job_type: "periodic" | "presubmit";
  repo: string;
  config_file?: string;
  config_revision?: string;
  current_status?: JobCurrentStatus;
  pass_rate_recent?: number;
  runs: BuildResult[];
  // Builds that have aged out of runs, newest-first. Display only: the backend
  // drops their test cases, and no analysis window includes them.
  retained_runs?: BuildResult[];
  pattern_analyses?: PatternAnalysis[];
  pattern_refresh?: PatternRefreshStatus;
  // Durable history for the failure signatures this window shows. Correlation
  // only ever sees one window, so this is where an infrequent flake's real age
  // comes from.
  failure_recurrence?: FailureRecurrence[];
}

// FailureRecurrence is one failure signature's history across build windows.
// occurrences counts distinct failing builds over the cause's lifetime, so it
// exceeds builds whenever the cause predates the current window.
export interface FailureRecurrence {
  signature: string;
  occurrences: number;
  first_seen: string;
  last_seen: string;
  builds?: string[];
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
