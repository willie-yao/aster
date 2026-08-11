import type {
  PatternRemediationInvestigationState,
  PatternRemediationInvestigationSummary,
} from "../types/dashboard";

export const notInvestigatedReason =
  "No source-grounded implementation target has been verified for this recurring cause.";

export interface PatternRemediationPresentation {
  state: PatternRemediationInvestigationState;
  label: string;
  message: string;
  futureAction?: "Investigate possible fix" | "Preview Fix PR";
  detail?: string;
}

const presentations: Record<
  PatternRemediationInvestigationState,
  Omit<PatternRemediationPresentation, "state" | "detail">
> = {
  not_investigated: {
    label: "Not investigated",
    message: notInvestigatedReason,
    futureAction: "Investigate possible fix",
  },
  investigating: {
    label: "Investigating",
    message: "A bounded read-only source investigation is in progress.",
  },
  actionable: {
    label: "Actionable",
    message: "A source-grounded implementation target passed deterministic verification.",
    futureAction: "Preview Fix PR",
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
    futureAction: "Investigate possible fix",
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
