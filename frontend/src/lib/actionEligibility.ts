import type { ActionEligibility } from "../types/actions";
import type { AIAnalysis, RemediationTarget } from "../types/dashboard";
import { buildActionsReady } from "./buildFailures.js";

const remediationExistsStatuses = new Set([
  "open",
  "awaiting_presubmit",
  "presubmit_running",
  "premerge_verified",
  "merged",
  "observing",
  "verified_fixed",
]);

export function eligibilityForState(
  state: ActionEligibility["state"],
): ActionEligibility {
  switch (state) {
    case "actionable":
      return {
        state,
        reason: "A verified implementation target remains at the pinned source commit.",
      };
    case "investigation_required":
      return {
        state,
        reason: "The published remediation requires source investigation before an issue or fix can be drafted.",
      };
    case "already_present":
      return {
        state,
        reason: "The grounded source already contains the proposed remediation.",
      };
    case "more_evidence_required":
      return {
        state,
        reason: "The published analysis does not contain enough verified source evidence for an implementation-ready action.",
      };
  }
}

function targetIsComplete(target: RemediationTarget): boolean {
  switch (target.intent) {
    case "add_symbol":
    case "modify_symbol":
      return Boolean(target.path?.trim() && target.symbol?.trim() && !target.value);
    case "set_configuration":
    case "remove_configuration":
      return Boolean(target.path?.trim() && target.value?.includes("=") && !target.symbol);
    case "investigate":
      return !target.path && !target.symbol && !target.value;
  }
}

export function patternActionEligibilityHint(
  targets: RemediationTarget[] | undefined,
  remediationStatus?: string,
): ActionEligibility | null {
  if (remediationStatus && remediationExistsStatuses.has(remediationStatus)) {
    return {
      state: "already_present",
      reason: "A remediation attempt already exists for this pattern.",
    };
  }
  if (!targets?.length) return eligibilityForState("more_evidence_required");
  if (!targets.every(targetIsComplete)) return eligibilityForState("more_evidence_required");
  if (targets.some((target) => target.intent === "investigate")) {
    return eligibilityForState("investigation_required");
  }
  return null;
}

export function buildActionEligibilityHint(
  analysis: AIAnalysis | undefined,
  currentCritiqueVersion: number | undefined,
): ActionEligibility | null {
  if (!buildActionsReady(analysis, currentCritiqueVersion)) {
    return eligibilityForState("more_evidence_required");
  }
  if (!analysis?.file_links || Object.keys(analysis.file_links).length === 0) {
    return eligibilityForState("more_evidence_required");
  }
  return null;
}

export function actionEligibilityTitle(
  eligibility: ActionEligibility,
  sourceInvestigationEnabled: boolean,
): string {
  switch (eligibility.state) {
    case "actionable":
      return "Actions available";
    case "already_present":
      return "Remediation already exists";
    case "more_evidence_required":
      return "More source evidence required";
    case "investigation_required":
      return sourceInvestigationEnabled
        ? "Investigate source"
        : "Source investigation is not configured";
  }
}
