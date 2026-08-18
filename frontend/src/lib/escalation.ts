// Shared escalation client primitives. Both escalation kinds run the same
// analysis under different subjects, so they share their wire types and the
// polling contract, and differ only in the endpoint they address.

export interface EscalationCitation {
  path: string;
  line_start?: number;
  line_end?: number;
  quote?: string;
}

export const API_BASE = import.meta.env?.BASE_URL ?? "/";

// Escalation lifecycle states, mirroring the prescalation package.
export type EscalationState =
  | "not_started"
  | "queued"
  | "running"
  | "complete"
  | "failed";

// EscalationView is one escalation's public state. The ref shape differs per
// subject, so it is left opaque here: the panel never reads it.
// EscalationEvidence names the build an analysis read, for subjects whose
// evidence build the server chose rather than the request naming it.
export interface EscalationEvidence {
  // A build is addressed by all three: a build id is unique within one
  // repository's pull request, not on its own.
  repo?: string;
  pull_number?: number;
  build_id?: string;
}

export interface EscalationView {
  state: EscalationState;
  root_cause?: string;
  severity?: string;
  suggested_fix?: string;
  citations?: EscalationCitation[];
  evidence?: EscalationEvidence;
  error?: string;
  started_at?: string;
  completed_at?: string;
}

// activeStates are the states worth polling.
const activeStates: EscalationState[] = ["queued", "running"];

export function escalationActive(state: EscalationState | undefined): boolean {
  return state !== undefined && activeStates.includes(state);
}

export async function readEscalation<T extends EscalationView>(url: string): Promise<T> {
  const response = await fetch(url, { credentials: "same-origin" });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<T>;
}

export async function postEscalation<T extends EscalationView>(
  url: string,
  idempotencyKey: string,
  body: string,
): Promise<T> {
  const response = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    body,
  });
  if (!response.ok) throw new Error(await safeError(response));
  return response.json() as Promise<T>;
}

async function safeError(response: Response): Promise<string> {
  const text = (await response.text()).trim();
  return text || `Escalation request failed with HTTP ${response.status}.`;
}
