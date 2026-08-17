import type {
  PatternCausalGroup,
  PatternRemediationInvestigationState,
  PatternRemediationInvestigationSummary,
} from "../types/dashboard";

export const notInvestigatedReason =
  "No source-grounded implementation target has been verified for this recurring cause.";

export const singleBuildRemediationReason =
  "Remediation investigation needs a cause repeated across at least two builds, so this single-build cause cannot be investigated.";

export const unhashedRemediationReason =
  "This causal group predates content hashing, so it cannot be addressed. Refresh the dashboard data to enable investigation.";

export const remediationUnavailableReason =
  "Remediation investigation is unavailable on this deployment.";

export interface PatternRemediationPresentation {
  state: PatternRemediationInvestigationState;
  label: string;
  message: string;
  detail?: string;
}

export interface CausalRemediationBlockedReason {
  label: string;
  message: string;
}

const presentations: Record<
  PatternRemediationInvestigationState,
  Omit<PatternRemediationPresentation, "state" | "detail">
> = {
  not_investigated: {
    label: "Not investigated",
    message: notInvestigatedReason,
  },
  queued: {
    label: "Queued",
    message: "The read-only remediation investigation is queued.",
  },
  investigating: {
    label: "Investigating",
    message: "A bounded read-only remediation investigation is in progress.",
  },
  verifying: {
    label: "Verifying",
    message: "The proposed target is undergoing deterministic verification.",
  },
  actionable: {
    label: "Actionable",
    message: "A source-grounded implementation target passed deterministic verification.",
  },
  already_fixed: {
    label: "Already fixed",
    message: "Current source already contains the verified remediation.",
  },
  external_dependency: {
    label: "External dependency",
    message: "The recurring cause belongs to a dependency outside the allowed destination repository.",
  },
  environment_or_infrastructure: {
    label: "Environment or infrastructure",
    message: "The recurring cause does not resolve to a verified repository change.",
  },
  mitigation_only: {
    label: "Mitigation only",
    message: "The available response is an operational mitigation, not a durable implementation target.",
  },
  insufficient_evidence: {
    label: "Insufficient evidence",
    message: "The investigation could not verify one unambiguous implementation target.",
  },
  failed: {
    label: "Investigation failed",
    message: "The read-only investigation did not produce a verified result. Published causal analysis is unchanged.",
  },
  stale: {
    label: "Stale",
    message: "This recurring cause is no longer the current active causal group. Refresh the dashboard before investigating again.",
  },
};

export function patternRemediationPresentation(
  investigation?: PatternRemediationInvestigationSummary,
): PatternRemediationPresentation {
  const requestedState = investigation?.state ?? "not_investigated";
  const state = requestedState in presentations ? requestedState : "not_investigated";
  const presentation = presentations[state];
  const detail = investigation?.reason?.trim();
  return {
    state,
    ...presentation,
    detail: detail && detail !== presentation.message ? detail : undefined,
  };
}

// States that only advance while the remediation operation is available. With
// the operation disabled they can never resolve, so reporting them verbatim
// would leave a permanently pending card the user cannot act on.
const unresolvableWithoutOperation = new Set<PatternRemediationInvestigationState>([
  "not_investigated",
  "queued",
  "investigating",
  "verifying",
]);

// causalRemediationBlockedReason reports why a causal group can never reach an
// investigation, so the UI never presents a pending-looking state as if the user
// could act on it. Conditions are ordered from the most permanent to the most
// deployment-scoped, and a terminal published verdict always wins because it
// carries a real result that outlives the capability that produced it.
export function causalRemediationBlockedReason(
  group: PatternCausalGroup,
  investigation: PatternRemediationInvestigationSummary | undefined,
  investigationEnabled: boolean,
): CausalRemediationBlockedReason | null {
  if (group.builds.length < 2) {
    return { label: "Not eligible", message: singleBuildRemediationReason };
  }
  if (!group.id || !group.content_hash) {
    return { label: "Not addressable", message: unhashedRemediationReason };
  }
  const { state } = patternRemediationPresentation(investigation);
  if (!investigationEnabled && unresolvableWithoutOperation.has(state)) {
    return { label: "Unavailable", message: remediationUnavailableReason };
  }
  return null;
}
