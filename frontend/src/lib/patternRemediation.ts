import type {
  PatternCausalGroup,
  PatternRemediationInvestigationState,
  PatternRemediationInvestigationSummary,
} from "../types/dashboard";

export const notInvestigatedReason =
  "No source-grounded implementation target has been verified for this cause.";

export const unhashedRemediationReason =
  "This causal group predates content hashing, so it cannot be addressed. Refresh the dashboard data to enable investigation.";

export const unrecurringPatternRemediationReason =
  "Remediation investigation runs on a recurring pattern, and these failures were not classified as one, so this cause cannot be investigated yet.";

export const remediationUnavailableReason =
  "Remediation investigation is unavailable on this deployment.";

export interface PatternRemediationPresentation {
  state: PatternRemediationInvestigationState;
  label: string;
  message: string;
  detail?: string;
}

// A block is either a verdict about this one cause or a capability the whole
// deployment lacks. The two mean unrelated things, so the scope travels with the
// reason and the UI can present them differently.
export type CausalRemediationBlockedScope = "cause" | "deployment";

export interface CausalRemediationBlockedReason {
  scope: CausalRemediationBlockedScope;
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
    message: "The cause belongs to a dependency outside the allowed destination repository.",
  },
  environment_or_infrastructure: {
    label: "Environment or infrastructure",
    message: "The cause does not resolve to a verified repository change.",
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
    message: "This cause is no longer the current active causal group. Refresh the dashboard before investigating again.",
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
//
// patternEligible mirrors the pattern-level requirement the resolver enforces:
// the investigation runs only on a recurring pattern, so a cause inside an
// unclassified one is reported here rather than offered as a control the server
// would reject.
export function causalRemediationBlockedReason(
  group: PatternCausalGroup,
  investigation: PatternRemediationInvestigationSummary | undefined,
  investigationEnabled: boolean,
  patternEligible = true,
): CausalRemediationBlockedReason | null {
  if (!group.id || !group.content_hash) {
    return { scope: "cause", label: "Not addressable", message: unhashedRemediationReason };
  }
  if (!patternEligible) {
    return { scope: "cause", label: "Not eligible", message: unrecurringPatternRemediationReason };
  }
  const { state } = patternRemediationPresentation(investigation);
  if (!investigationEnabled && unresolvableWithoutOperation.has(state)) {
    // The label names the scope so it cannot be mistaken for a verdict about
    // this cause when both appear on the same briefing.
    return {
      scope: "deployment",
      label: "Unavailable on this deployment",
      message: remediationUnavailableReason,
    };
  }
  return null;
}
