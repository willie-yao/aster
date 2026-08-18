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

export interface EscalationRef {
  pullNumber: number;
  jobID: string;
  buildID: string;
  testName: string;
}

// PullRequestEscalationView is one pull request escalation's state.
export interface PullRequestEscalationView extends EscalationView {
  ref: {
    pull_number: number;
    job_id: string;
    build_id: string;
    test_name: string;
  };
}

function endpoint(ref: EscalationRef): string {
  return (
    `${API_BASE}api/pull-requests/${encodeURIComponent(String(ref.pullNumber))}` +
    `/checks/${encodeURIComponent(ref.jobID)}` +
    `/builds/${encodeURIComponent(ref.buildID)}/escalation`
  );
}

export function getEscalation(ref: EscalationRef): Promise<PullRequestEscalationView> {
  const query = new URLSearchParams({ test: ref.testName });
  return readEscalation<PullRequestEscalationView>(`${endpoint(ref)}?${query}`);
}

export function startEscalation(
  ref: EscalationRef,
  idempotencyKey: string,
): Promise<PullRequestEscalationView> {
  // A Ginkgo test name contains slashes and spaces, so it travels in the body
  // rather than a path segment.
  return postEscalation<PullRequestEscalationView>(
    endpoint(ref),
    idempotencyKey,
    JSON.stringify({ test_name: ref.testName }),
  );
}
