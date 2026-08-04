export type AnalysisChatAssessment =
  | "explains"
  | "supports"
  | "challenges"
  | "inconclusive";

interface AnalysisChatReferenceBase {
  job_id: string;
}

export interface JUnitAnalysisChatReference extends AnalysisChatReferenceBase {
  scope?: "test";
  build_id: string;
  test_name: string;
  source?: never;
  suite_name?: string;
  class_name?: string;
  junit_file?: string;
  analysis_generated_at?: string;
  pattern_id?: never;
  pattern_hash?: never;
}

export interface BuildAnalysisChatReference extends AnalysisChatReferenceBase {
  scope?: "test";
  build_id: string;
  test_name: string;
  source: "build";
  suite_name?: string;
  class_name?: string;
  junit_file?: never;
  analysis_generated_at?: string;
  pattern_id?: never;
  pattern_hash?: never;
}

export type TestAnalysisChatReference = JUnitAnalysisChatReference | BuildAnalysisChatReference;

export interface PatternAnalysisChatReference extends AnalysisChatReferenceBase {
  scope: "pattern";
  pattern_id: string;
  pattern_hash: string;
  build_id?: never;
  test_name?: never;
  source?: never;
  suite_name?: never;
  class_name?: never;
  junit_file?: never;
  analysis_generated_at?: never;
}

export type AnalysisChatReference = TestAnalysisChatReference | PatternAnalysisChatReference;

export interface AnalysisChatCitation {
  path: string;
  line_start?: number;
  line_end?: number;
  quote?: string;
}

export interface AnalysisChatRevision {
  root_cause: string;
  suggested_fix: string;
}

export interface AnalysisChatMessage {
  role: "user" | "assistant";
  request_id?: string;
  content: string;
  assessment?: AnalysisChatAssessment;
  citations?: AnalysisChatCitation[];
  proposed_revision?: AnalysisChatRevision;
  tool_calls?: number;
  gcs_bytes?: number;
  elapsed_ms?: number;
  created_at: string;
}

export type AnalysisChatAttemptOutcome =
  | "pending"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "timed_out"
  | "unknown";

export type AnalysisChatAttemptFailureKind =
  | "model"
  | "provider"
  | "validation"
  | "citation"
  | "source";

export interface AnalysisChatAttempt {
  request_id: string;
  question?: string;
  outcome: AnalysisChatAttemptOutcome;
  failure_kind?: AnalysisChatAttemptFailureKind;
  turn?: number;
  created_at?: string;
  updated_at?: string;
}

export interface AnalysisChatSession {
  id: string;
  analysis: AnalysisChatReference;
  created_at: string;
  updated_at: string;
  expires_at: string;
  messages: AnalysisChatMessage[];
  attempts?: AnalysisChatAttempt[];
  active?: AnalysisChatActiveTurn;
  turns_used: number;
  max_turns: number;
}

export type AnalysisChatProgressPhase =
  | "queued"
  | "investigating"
  | "reading_evidence"
  | "evaluating"
  | "finalizing"
  | "validation_retrying"
  | "cancelling";

export interface AnalysisChatProgress {
  request_id: string;
  phase: AnalysisChatProgressPhase;
  started_at?: string;
  updated_at: string;
  turns_used?: number;
  max_turns?: number;
  validation_retries?: number;
  max_validation_retries?: number;
}

export interface AnalysisChatActiveTurn extends AnalysisChatProgress {
  question?: string;
}
