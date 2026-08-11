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
  | "contract_generation_failed"
  | "unsafe_remediation"
  | "already_present"
  | "source_verification_inconclusive"
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

export interface ActionPreview {
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
  result_url?: string;
  superseded_by?: string;
  preview?: ActionPreview;
  email_sent?: boolean;
}
