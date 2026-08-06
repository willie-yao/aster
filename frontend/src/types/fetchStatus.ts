export type FetchPassType = "one-shot" | "initial-watch" | "lightweight-watch" | "reconcile";

export type FetchPhase =
  | "setup"
  | "discovery"
  | "artifacts"
  | "aggregation"
  | "analysis-planning"
  | "analysis"
  | "patterns"
  | "publication"
  | "side-effects"
  | "idle"
  | "complete"
  | "failed"
  | "cancelled"
  | "interrupted";

export type FetchOutcome = "running" | "succeeded" | "failed" | "cancelled" | "interrupted";
export type FetchStageState = "pending" | "running" | "completed" | "skipped" | "failed" | "cancelled";
export type FetchStatusState = "missing" | "unavailable" | "active" | "idle" | "completed" | "failed" | "cancelled" | "interrupted" | "stale";

export interface FetchProgressStatus {
  schema_version: number;
  run_id: string;
  pass_id: string;
  pass_type: FetchPassType;
  engine_version?: string;
  phase: FetchPhase;
  run_started_at: string;
  pass_started_at: string;
  phase_started_at: string;
  last_progress_at: string;
  last_checked_at?: string;
  last_successful_publication_at?: string;
  outcome: FetchOutcome;
  failure_category?: string;
  jobs: { total: number; completed: number };
  builds: { cached: number; fetched: number };
  analyses: {
    logical_total: number;
    accepted_cache_hits: number;
    compatible_results_reused?: number;
    exact_results_reused?: number;
    same_failure_results_reused?: number;
    same_failure_groups?: number;
    same_failure_candidates?: number;
    potential_tasks_saved?: number;
    largest_same_failure_group?: number;
    new_work: number;
    stale_work: number;
    cache_rejections?: {
      missing: number;
      expired: number;
      tool_floor: number;
      evidence_floor: number;
      critique: number;
      malformed: number;
    };
    queued: number;
    running: number;
    completed: number;
    failed: number;
    cancelled: number;
    task_attempts: number;
    retries: number;
    existing_tasks_adopted: number;
    new_tasks_created?: number;
    results_retrieved: number;
    fresh_analyses_completed?: number;
    result_retrieval_retries: number;
    checkpoint_committed?: boolean;
    build_subjects?: {
      logical_total: number;
      queued: number;
      running: number;
      completed: number;
      failed: number;
      cancelled: number;
      accepted_cache_hits: number;
      exact_results_reused?: number;
      existing_tasks_adopted: number;
      new_tasks_created?: number;
      fresh_analyses_completed?: number;
    };
  };
  patterns?: {
    eligible: number;
    completed: number;
    failed: number;
    attempts: number;
    retries: number;
    cache_hits?: number;
    repairs?: number;
    repair_succeeded?: number;
    repair_failed?: number;
    repair_failure_category?: string;
    failure_category?: string;
    current?: number;
    retained?: number;
    unavailable?: number;
  };
  source_grounding?: {
    configured: boolean;
    mode?: "anonymous" | "authenticated";
    owner?: string;
    repository?: string;
    ref_strategy?: string;
  };
  skill_bundle?: {
    profiles?: string[];
    engine_count: number;
    consumer_count: number;
    consumer_bundle_present: boolean;
    ids?: string[];
    hash?: string;
  };
  pattern_phase: FetchStageState;
  publication_phase: FetchStageState;
  side_effect_phase: FetchStageState;
  phase_durations_ms?: Record<string, number>;
  next_watch_at?: string;
  next_reconcile_at?: string;
}

export interface FetchPassSummary {
  pass_type: FetchPassType;
  started_at: string;
  completed_at: string;
  duration_ms: number;
  logical_count: number;
  cache_hits: number;
  compatible_results_reused: number;
  exact_results_reused: number;
  same_failure_results_reused?: number;
  same_failure_groups?: number;
  same_failure_candidates?: number;
  potential_tasks_saved?: number;
  largest_same_failure_group?: number;
  new_tasks_created: number;
  fresh_analyses_completed: number;
  retries: number;
  outcome: FetchOutcome;
  published: boolean;
}

export interface FetchStatusResponse {
  available: boolean;
  state: FetchStatusState;
  stale?: boolean;
  status?: FetchProgressStatus;
  history_schema_version?: number;
  history?: FetchPassSummary[];
}
