import type { ActionEligibility, ActionReasonCode } from "../types/actions";
import type { AIAnalysis, PatternAnalysis, PatternCausalGroup, PatternLifecycle, PatternRefreshStatus, RemediationTarget } from "../types/dashboard";
import { buildActionsReady } from "./buildFailures.js";

const stateCode: Record<ActionEligibility["state"], ActionReasonCode> = {
  actionable: "actionable",
  investigation_required: "investigation_required",
  already_present: "already_present",
  recovered: "recovered",
  more_evidence_required: "evidence_unavailable",
};

const reasonMessages: Record<ActionReasonCode, string> = {
  actionable: "A verified implementation target remains at the pinned source commit.",
  recovered: "Observed passing runs have recovered, but source verification has not proven a fix.",
  observing: "The remediation is present and the dashboard is observing later comparable runs.",
  verified_fixed: "The remediation and multiple later passing runs have been verified at pinned source revisions.",
  non_systemic: "This result was classified as non-systemic and does not qualify for a recurring-pattern action.",
  evidence_unavailable: "Current published evidence is unavailable or no longer matches the selected action subject.",
  investigation_required: "The published remediation requires maintainer investigation before an issue or fix can be drafted.",
  no_reviewable_patch: "No reviewable patch was generated. Add a maintainer instruction and regenerate.",
  contract_generation_failed: "The action preview could not be generated from the current verified inputs.",
  unsafe_remediation: "The proposed remediation violates the deterministic safety policy and requires further investigation.",
  already_present: "The verified source already contains the proposed remediation.",
  source_verification_inconclusive: "Pinned-source verification was inconclusive; investigate the verified source before starting an action.",
  source_branch_unknown: "The build does not report a resolvable source branch, so a generation base cannot be established.",
  source_revision_diverged: "The failure commit is not an ancestor of its branch head, so a patch cannot be safely generated.",
  source_changed: "A verified source path is unavailable or changed between the failure revision and its branch head.",
  provider_credential_rejected:
    "The model provider rejected the sandbox credential. Generation cannot succeed until the credential is fixed.",
  generation_failed: "Draft generation did not complete successfully.",
};

function stateForCode(code: ActionReasonCode): ActionEligibility["state"] {
  if (code === "actionable") return "actionable";
  if (code === "investigation_required") return "investigation_required";
  if (code === "recovered") return "recovered";
  if (code === "already_present" || code === "observing" || code === "verified_fixed") return "already_present";
  return "more_evidence_required";
}

export function eligibilityForCode(
  code: ActionReasonCode,
  reason = reasonMessages[code],
): ActionEligibility {
  return { state: stateForCode(code), code, reason };
}

export function eligibilityForState(
  state: ActionEligibility["state"],
): ActionEligibility {
  return eligibilityForCode(stateCode[state]);
}

export function selectActionEligibility(
  hint: ActionEligibility | null | undefined,
  fetched: { failureID: string; value: ActionEligibility } | null,
  failureID: string,
): ActionEligibility | null {
  if (hint != null) return hint;
  return fetched?.failureID === failureID ? fetched.value : null;
}

export function normalizeActionEligibility(value: ActionEligibility): ActionEligibility {
  return value.code ? value : { ...value, code: stateCode[value.state] };
}

export function patternLifecycleActive(lifecycle: PatternLifecycle | undefined): boolean {
  return !lifecycle || lifecycle.state === "active";
}

// patternRefreshBlockingCode mirrors the server's patternRefreshReasonCode.
// Whether the correlation produced a fresh result this pass is not itself
// disqualifying: a retained pattern is still the published subject, so what
// decides is whether the evidence behind it is readable. Kept as one predicate
// because the hint, dismissal, and drafting checks all have to agree with the
// server and with each other.
function patternRefreshBlockingCode(
  refreshStatus?: PatternRefreshStatus,
): ActionReasonCode | null {
  if (!refreshStatus) return null;
  if (refreshStatus.state === "failed") return "contract_generation_failed";
  if (
    refreshStatus.state !== "current" &&
    refreshStatus.state !== "retained" &&
    refreshStatus.state !== "unavailable"
  ) {
    return "evidence_unavailable";
  }
  return refreshStatus.evidence_available ? null : "evidence_unavailable";
}

// patternActionRefreshBlocked reports whether a pattern's correlation state
// blocks a pattern-scope action, mirroring the server.
export function patternActionRefreshBlocked(refreshStatus?: PatternRefreshStatus): boolean {
  return patternRefreshBlockingCode(refreshStatus) !== null;
}

// patternResolvable reports whether a maintainer can resolve a whole pattern.
// Resolution acknowledges every cause at once rather than starting a
// remediation-contract action, so it deliberately ignores
// recurrence_classification: causal-group results are resolvable too. The
// remaining checks mirror the server, which needs readable evidence, a systemic
// and lifecycle-active pattern, and at least one shared build to use as the
// recurrence watermark.
export function patternResolvable(
  pattern: PatternAnalysis,
  refreshStatus?: PatternRefreshStatus,
): boolean {
  if (patternRefreshBlockingCode(refreshStatus)) {
    return false;
  }
  return Boolean(
    pattern.id &&
    pattern.systemic &&
    patternLifecycleActive(pattern.lifecycle) &&
    // resolve.Watermark parses build ids as decimal integers. This is
    // deliberately stricter (unsigned only): being wrong hides a control the
    // server would have accepted, rather than offering one it will reject.
    pattern.shared_builds?.some((buildID) => /^\d+$/.test(buildID.trim())),
  );
}

// causeResolvable reports whether a maintainer can resolve one cause on its own.
// It mirrors patternResolvable's server checks, then adds the signature
// requirement: a per-cause resolution is keyed by the causal group's signature,
// so a group the engine could not sign has no durable identity to record one
// under and is covered by the pattern-level acknowledgement instead.
export function causeResolvable(
  pattern: PatternAnalysis,
  group: PatternCausalGroup,
  refreshStatus?: PatternRefreshStatus,
): boolean {
  if (patternRefreshBlockingCode(refreshStatus)) {
    return false;
  }
  return Boolean(
    group.signature?.trim() &&
    pattern.systemic &&
    patternLifecycleActive(pattern.lifecycle) &&
    // resolve.CauseWatermark parses build ids as decimal integers, matching the
    // stricter read patternResolvable takes for the same reason.
    group.builds?.some((buildID) => /^\d+$/.test(buildID.trim())),
  );
}

// patternResolutionCovered reports whether every cause of a pattern can be
// resolved individually, which is when the pattern-level control is redundant
// and stands down in favour of the per-cause ones. A pattern with no causal
// groups is never covered, so it keeps the pattern-level control.
export function patternResolutionCovered(
  pattern: PatternAnalysis,
  refreshStatus?: PatternRefreshStatus,
): boolean {
  const groups = pattern.causal_groups ?? [];
  return groups.length > 0 && groups.every((group) => causeResolvable(pattern, group, refreshStatus));
}

// patternDraftable reports whether pattern-level issue and fix-PR drafting
// applies. Causal-group results publish per-cause remediation instead of a
// pattern-level remediation contract, so they never qualify. This is
// independent of patternResolvable: a pattern can be draftable but not
// dismissible, or the reverse.
export function patternDraftable(
  pattern: PatternAnalysis,
  refreshStatus?: PatternRefreshStatus,
): boolean {
  return Boolean(
    !pattern.recurrence_classification &&
    !patternRefreshBlockingCode(refreshStatus) &&
    pattern.id &&
    pattern.systemic &&
    patternLifecycleActive(pattern.lifecycle),
  );
}

function targetIsComplete(target: RemediationTarget): boolean {
  switch (target.intent) {
    case "add_symbol":
      return Boolean(target.path?.trim() && target.symbol?.trim() && !target.value);
    case "modify_symbol":
      return Boolean(target.path?.trim() && target.symbol?.trim() && target.required_call?.trim() && !target.value);
    case "set_configuration":
    case "remove_configuration":
      return Boolean(target.path?.trim() && target.value?.includes("=") && !target.symbol);
    case "set_job_environment":
      return Boolean(
        target.repository === "kubernetes/test-infra" &&
        /^[0-9a-f]{40}$/.test(target.revision ?? "") &&
        target.path?.startsWith("config/jobs/") &&
        target.job?.trim() &&
        target.container?.trim() &&
        /^[A-Za-z_][A-Za-z0-9_]*$/.test(target.name ?? "") &&
        target.value?.trim() &&
        !target.symbol,
      );
    case "investigate":
      return !target.path && !target.symbol && !target.value;
  }
}

export function patternActionEligibilityHint(
  targets: RemediationTarget[] | undefined,
  lifecycle?: PatternLifecycle,
  systemic = true,
  refreshStatus?: PatternRefreshStatus,
): ActionEligibility | null {
  const refreshBlocked = patternRefreshBlockingCode(refreshStatus);
  if (refreshBlocked) return eligibilityForCode(refreshBlocked);
  if (!systemic) return eligibilityForCode("non_systemic");
  if (!patternLifecycleActive(lifecycle)) {
    const code = lifecycle?.state === "recovered"
      ? "recovered"
      : lifecycle?.state === "observing"
        ? "observing"
        : "verified_fixed";
    return eligibilityForCode(code, lifecycle?.reason ?? reasonMessages[code]);
  }
  if (!targets?.length) return eligibilityForCode("contract_generation_failed");
  if (!targets.every(targetIsComplete)) return eligibilityForCode("contract_generation_failed");
  if (targets.some((target) => target.intent === "investigate")) {
    return eligibilityForCode("investigation_required");
  }
  return null;
}

export function buildActionEligibilityHint(
  analysis: AIAnalysis | undefined,
  currentCritiqueVersion: number | undefined,
): ActionEligibility | null {
  if (!buildActionsReady(analysis, currentCritiqueVersion)) {
    return eligibilityForCode("contract_generation_failed");
  }
  if (!analysis?.file_links || Object.keys(analysis.file_links).length === 0) {
    return eligibilityForCode("evidence_unavailable");
  }
  return null;
}

export function actionEligibilityTitle(eligibilityValue: ActionEligibility): string {
  const eligibility = normalizeActionEligibility(eligibilityValue);
  switch (eligibility.code) {
    case "actionable":
      return "Actions available";
    case "already_present":
      return "Remediation already exists";
    case "recovered":
      return "Watching recovery";
    case "observing":
      return "Observing verified remediation";
    case "verified_fixed":
      return "Verified fixed";
    case "non_systemic":
      return "Not a recurring systemic pattern";
    case "evidence_unavailable":
      return "Current evidence unavailable";
    case "contract_generation_failed":
      return "Preview generation failed";
    case "unsafe_remediation":
      return "Unsafe remediation blocked";
    case "source_verification_inconclusive":
      return "Source verification inconclusive";
    case "generation_failed":
      return "Draft generation failed";
    case "provider_credential_rejected":
      return "Model provider credential rejected";
    case "investigation_required":
      return "Investigation required";
    default:
      return "Action unavailable";
  }
}
