export type Action = "create-issue" | "propose-fix";

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
  warning?: string;
  result_url?: string;
  superseded_by?: string;
  preview?: ActionPreview;
  email_sent?: boolean;
}
