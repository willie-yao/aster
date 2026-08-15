export type SourceInvestigationStatus = "pending" | "succeeded" | "failed" | "unknown";

export type SourceInvestigationPhase =
  | "queued"
  | "cloning_source"
  | "investigating_source"
  | "verifying_citations"
  | "finalizing"
  | "cancelling";

export type SourceInvestigationConfidence = "high" | "medium" | "low";

export type SourceInvestigationRelationship =
  | "supports"
  | "refines"
  | "contradicts"
  | "inconclusive";

export interface SourceInvestigationCitation {
  path: string;
  line_start: number;
  line_end: number;
  quote: string;
  verified: boolean;
}

export type SourceInvestigationState =
  | "already_present"
  | "actionable_code_change"
  | "actionable_configuration_change"
  | "inconclusive";

export interface SourceInvestigationResult {
  state?: SourceInvestigationState;
  target?: { intent: string; path?: string; symbol?: string; value?: string };
  finding: string;
  confidence: SourceInvestigationConfidence;
  relationship: SourceInvestigationRelationship;
  direction: string;
  citations?: SourceInvestigationCitation[];
  elapsed_ms?: number;
}

export interface SourceInvestigationView {
  id: string;
  session_id: string;
  chat_request_id: string;
  status: SourceInvestigationStatus;
  phase?: SourceInvestigationPhase;
  created_at: string;
  updated_at: string;
  expires_at: string;
  result?: SourceInvestigationResult;
}

export interface SourceInvestigationProgress {
  request_id: string;
  phase: SourceInvestigationPhase;
  updated_at: string;
}
