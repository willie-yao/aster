// EscalationCitation is one artifact range supporting the analysis.
export interface EscalationCitation {
  path: string;
  line_start?: number;
  line_end?: number;
  quote?: string;
}

const API_BASE = import.meta.env?.BASE_URL ?? "/";

// Escalation lifecycle states, mirroring the prescalation package.
export type EscalationState =
  | "not_started"
  | "queued"
  | "running"
  | "complete"
  | "failed";

export interface EscalationRef {
  pullNumber: number;
  jobID: string;
  buildID: string;
  testName: string;
}

export interface EscalationView {
  ref: {
    pull_number: number;
    job_id: string;
    build_id: string;
    test_name: string;
  };
  state: EscalationState;
  root_cause?: string;
  severity?: string;
  suggested_fix?: string;
  citations?: EscalationCitation[];
  error?: string;
  started_at?: string;
  completed_at?: string;
}

// activeEscalationStates are the states worth polling.
const activeStates: EscalationState[] = ["queued", "running"];

export function escalationActive(state: EscalationState | undefined): boolean {
  return state !== undefined && activeStates.includes(state);
}

function endpoint(ref: EscalationRef): string {
  return (
    `${API_BASE}api/pull-requests/${encodeURIComponent(String(ref.pullNumber))}` +
    `/checks/${encodeURIComponent(ref.jobID)}` +
    `/builds/${encodeURIComponent(ref.buildID)}/escalation`
  );
}

export async function getEscalation(ref: EscalationRef): Promise<EscalationView> {
  const query = new URLSearchParams({ test: ref.testName });
  const response = await fetch(`${endpoint(ref)}?${query}`, { credentials: "same-origin" });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<EscalationView>;
}

export async function startEscalation(
  ref: EscalationRef,
  idempotencyKey: string,
): Promise<EscalationView> {
  const response = await fetch(endpoint(ref), {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body: JSON.stringify({ test_name: ref.testName }),
  });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<EscalationView>;
}

async function safeError(response: Response): Promise<string> {
  const text = (await response.text()).trim();
  return text || `Escalation request failed with HTTP ${response.status}.`;
}
