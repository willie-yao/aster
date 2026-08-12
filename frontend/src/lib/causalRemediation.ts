import type { PatternRemediationInvestigationSummary } from "../types/dashboard";

const API_BASE = import.meta.env.BASE_URL;

export interface CausalRemediationRef {
  jobID: string;
  patternID: string;
  patternHash: string;
  causalGroupID: string;
  causalGroupHash: string;
}

function endpoint(ref: CausalRemediationRef): string {
  return `${API_BASE}api/jobs/${encodeURIComponent(ref.jobID)}/patterns/${encodeURIComponent(ref.patternID)}/causal-groups/${encodeURIComponent(ref.causalGroupID)}/remediation-investigation`;
}

export async function getCausalRemediationStatus(
  ref: CausalRemediationRef,
): Promise<PatternRemediationInvestigationSummary> {
  const query = new URLSearchParams({
    pattern_hash: ref.patternHash,
    causal_group_hash: ref.causalGroupHash,
  });
  const response = await fetch(`${endpoint(ref)}?${query}`, { credentials: "same-origin" });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<PatternRemediationInvestigationSummary>;
}

export async function startCausalRemediation(
  ref: CausalRemediationRef,
  idempotencyKey: string,
  refresh = false,
): Promise<PatternRemediationInvestigationSummary> {
  const response = await fetch(endpoint(ref), {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify({
      pattern_hash: ref.patternHash,
      causal_group_hash: ref.causalGroupHash,
      refresh,
    }),
  });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<PatternRemediationInvestigationSummary>;
}

async function safeError(response: Response): Promise<string> {
  const text = (await response.text()).trim();
  return text || `Remediation investigation request failed with HTTP ${response.status}.`;
}
