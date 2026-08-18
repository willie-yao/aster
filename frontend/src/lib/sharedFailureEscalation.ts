import {
  API_BASE,
  postEscalation,
  readEscalation,
  type EscalationView,
} from "./escalation.js";

export type {
  EscalationCitation,
  EscalationEvidence,
  EscalationState,
  EscalationView,
} from "./escalation.js";
export { escalationActive } from "./escalation.js";

// SharedFailureEscalationView is one shared failure escalation's state. The
// subject is the published cluster id, which already encodes the correlation
// key, so nothing else identifies it.
export interface SharedFailureEscalationView extends EscalationView {
  ref: { id: string };
}

function endpoint(id: string): string {
  return `${API_BASE}api/shared-failures/${encodeURIComponent(id)}/escalation`;
}

export function getSharedFailureEscalation(id: string): Promise<SharedFailureEscalationView> {
  return readEscalation<SharedFailureEscalationView>(endpoint(id));
}

export function startSharedFailureEscalation(
  id: string,
  idempotencyKey: string,
): Promise<SharedFailureEscalationView> {
  // The subject is entirely in the path, so the body carries nothing. The
  // server rejects a body with content rather than silently misreading it.
  return postEscalation<SharedFailureEscalationView>(endpoint(id), idempotencyKey, "{}");
}
