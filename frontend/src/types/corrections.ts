import type { AnalysisChatCitation, AnalysisChatReference, AnalysisChatRevision } from "./analysisChat";

export type AnalysisCorrectionStatus = "active" | "revoked";

export interface AnalysisCorrectionPreview {
  token: string;
  analysis: AnalysisChatReference;
  original: AnalysisChatRevision;
  proposed: AnalysisChatRevision;
  citations: AnalysisChatCitation[];
  expires_at: string;
}

export interface AnalysisCorrection {
  id: string;
  status: AnalysisCorrectionStatus;
  analysis: AnalysisChatReference;
  revision: AnalysisChatRevision;
  citations: AnalysisChatCitation[];
  corrected_by: string;
  corrected_at: string;
  revoked_by?: string;
  revoked_at?: string;
}

export interface AnalysisCorrectionState {
  corrections: Record<string, AnalysisCorrection>;
}
