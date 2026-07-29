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
    new_work: number;
    stale_work: number;
    queued: number;
    running: number;
    completed: number;
    failed: number;
    cancelled: number;
    task_attempts: number;
    retries: number;
    existing_tasks_adopted: number;
    results_retrieved: number;
    result_retrieval_retries: number;
  };
  patterns?: {
    eligible: number;
    completed: number;
    failed: number;
    attempts: number;
    retries: number;
    failure_category?: string;
  };
  pattern_phase: string;
  publication_phase: string;
  side_effect_phase: string;
  phase_durations_ms?: Record<string, number>;
  next_watch_at?: string;
  next_reconcile_at?: string;
}

export interface FetchStatusResponse {
  available: boolean;
  state: FetchStatusState;
  stale?: boolean;
  status?: FetchProgressStatus;
}
