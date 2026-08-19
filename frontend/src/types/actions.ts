export type Action = "create-issue" | "propose-fix";

export type ActionReasonCode =
  | "actionable"
  | "recovered"
  | "observing"
  | "verified_fixed"
  | "retained_stale"
  | "non_systemic"
  | "evidence_unavailable"
  | "investigation_required"
  | "no_reviewable_patch"
  | "contract_generation_failed"
  | "unsafe_remediation"
  | "already_present"
  | "source_verification_inconclusive"
  | "source_branch_unknown"
  | "source_revision_diverged"
  | "source_changed"
  | "generation_failed";

export interface ActionEligibility {
  state:
    | "actionable"
    | "investigation_required"
    | "already_present"
    | "recovered"
    | "more_evidence_required";
  code?: ActionReasonCode;
  reason: string;
}

export type RequestStatus =
  | "pending"
  | "cancelling"
  | "ready"
  | "unknown"
  | "failed"
  | "confirmed"
  | "cancelled"
  | "expired";

export type RequestStage = "verifying_remediation" | "drafting";

export interface ActionVerification {
  state: "unresolved" | "already_present" | "inconclusive";
  code?: ActionReasonCode;
  reason: string;
}

export type AnalysisFixFailureCategory =
  | "no_reviewable_patch"
  | "runtime_infrastructure"
  | "result_contract"
  | "safety_integrity"
  | "source_changed"
  | "cancelled"
  | "timed_out";

export interface SafeCommandResult {
  argv: string[];
  exit_code: number;
  duration_ms: number;
  timed_out?: boolean;
}

export interface AnalysisFixFailure {
  category: AnalysisFixFailureCategory;
  terminal_state?: "succeeded" | "failed" | "timed_out" | "cancelled";
  command_results?: SafeCommandResult[];
  changed_files?: string[];
}

export interface ActionPreview {
  token?: string;
  kind: "issue" | "fix";
  title: string;
  body: string;
  diff?: string;
  verify_status?: string;
  verify_summary?: string;
  verify_output?: string;
}

export interface ActionRequest {
  id: string;
  failure_id: string;
  kind: Action;
  owner: string;
  status: RequestStatus;
  stage?: RequestStage;
  verification?: ActionVerification;
  created_at: string;
  updated_at: string;
  expires_at: string;
  error?: string;
  reason_code?: ActionReasonCode;
  warning?: string;
  failure?: AnalysisFixFailure;
  result_url?: string;
  superseded_by?: string;
  preview?: ActionPreview;
  email_sent?: boolean;
}
