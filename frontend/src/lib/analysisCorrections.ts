import type { AnalysisChatReference } from "../types/analysisChat";
import type { AnalysisCorrection, AnalysisCorrectionPreview, AnalysisCorrectionState } from "../types/corrections";

const API_BASE = import.meta.env.BASE_URL;

export class AnalysisCorrectionAPIError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "AnalysisCorrectionAPIError";
    this.status = status;
  }
}

async function parse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim();
    throw new AnalysisCorrectionAPIError(response.status, body || `Analysis correction request failed with HTTP ${response.status}`);
  }
  return response.json() as Promise<T>;
}

export async function previewAnalysisCorrection(sessionID: string, requestID: string, signal?: AbortSignal): Promise<AnalysisCorrectionPreview> {
  const response = await fetch(`${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/requests/${encodeURIComponent(requestID)}/correction/preview`, {
    method: "POST", credentials: "same-origin", cache: "no-store", signal,
  });
  return parse<AnalysisCorrectionPreview>(response);
}

export async function confirmAnalysisCorrection(token: string, signal?: AbortSignal): Promise<AnalysisCorrection> {
  const response = await fetch(`${API_BASE}api/analysis-corrections/confirm`, {
    method: "POST", credentials: "same-origin", cache: "no-store", signal,
    headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token }),
  });
  return parse<AnalysisCorrection>(response);
}

export async function revokeAnalysisCorrection(correctionID: string, signal?: AbortSignal): Promise<AnalysisCorrection> {
  const response = await fetch(`${API_BASE}api/analysis-corrections/${encodeURIComponent(correctionID)}/revoke`, {
    method: "POST", credentials: "same-origin", cache: "no-store", signal,
  });
  return parse<AnalysisCorrection>(response);
}

export function findAnalysisCorrection(state: AnalysisCorrectionState, ref: AnalysisChatReference): AnalysisCorrection | undefined {
  return Object.values(state.corrections).find((correction) => {
    const candidate = correction.analysis;
    return candidate.job_id === ref.job_id && candidate.build_id === ref.build_id &&
      candidate.test_name === ref.test_name && (candidate.source ?? "") === (ref.source ?? "") && (candidate.suite_name ?? "") === (ref.suite_name ?? "") &&
      (candidate.class_name ?? "") === (ref.class_name ?? "") && (candidate.junit_file ?? "") === (ref.junit_file ?? "");
  });
}
