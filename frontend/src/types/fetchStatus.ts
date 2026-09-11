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
export type FetchFollowUpState = "running" | "completed" | "skipped" | "disabled" | "failed" | "cancelled";
export type FetchFollowUpReason = "not-configured" | "no-work" | "dependency-failed";

export interface FetchFollowUpComponent {
  state: FetchFollowUpState;
  reason?: FetchFollowUpReason;
  code?: string;
  summary?: string;
}

export interface FetchFollowUpProgress {
  notifications?: FetchFollowUpComponent;
  automatic_issues?: FetchFollowUpComponent;
}

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
    queued: number;
    running: number;
    completed: number;
    failed: number;
    cancelled: number;
    checkpoint_committed?: boolean;
    build_subjects?: {
      logical_total: number;
      queued: number;
      running: number;
      completed: number;
      failed: number;
      cancelled: number;
    };
  };
  patterns?: {
    eligible: number;
    completed: number;
    failed: number;
    attempts: number;
    retries: number;
    cache_hits?: number;
    suppressed?: number;
    fresh_retries?: number;
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
  follow_up?: FetchFollowUpProgress;
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
