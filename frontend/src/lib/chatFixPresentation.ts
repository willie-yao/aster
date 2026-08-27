import type { ChatFixRequest } from "./chatFix.js";

export interface ChatFixRequestPresentation {
  severity: "info" | "warning" | "error";
  message: string;
  canRegenerate: boolean;
  shouldObserve: boolean;
}

export function chatFixRequestPresentation(request: ChatFixRequest): ChatFixRequestPresentation | null {
  if (request.status === "pending" || request.status === "cancelling" || request.status === "unknown") {
    return {
      severity: "info",
      message: "This admitted request is still being observed. Reopening the dialog continues with the same request ID.",
      canRegenerate: false,
      shouldObserve: true,
    };
  }
  if (request.status === "failed") {
    const canRegenerate = request.failure?.category === "no_reviewable_patch";
    return {
      severity: canRegenerate ? "warning" : "error",
      message: failureMessage(request),
      canRegenerate,
      shouldObserve: false,
    };
  }
  return null;
}

function failureMessage(request: ChatFixRequest): string {
  switch (request.failure?.category) {
    case "no_reviewable_patch":
      return noReviewablePatchMessage(request);
    case "runtime_infrastructure":
      return "Fix preview generation failed in the isolated runtime.";
    case "provider_credential":
      return "The model provider rejected the sandbox credential. Fix the credential before requesting another preview.";
    case "result_contract":
      return "Fix preview generation returned an invalid result contract.";
    case "safety_integrity":
      return "Fix preview generation was blocked by a safety or integrity check.";
    case "source_changed":
      return "The verified source or generation base changed before the preview completed.";
    case "cancelled":
      return "Fix preview generation was cancelled.";
    case "timed_out":
      return "Fix preview generation timed out.";
    default:
      return request.error || "Fix preview generation failed a safety, integrity, or runtime check.";
  }
}

function noReviewablePatchMessage(request: ChatFixRequest): string {
  const detail = request.error || "The coding agent did not return a reviewable repository change.";
  if (request.failure?.detail === "review_scope_exceeded") {
    return `${detail} Add a narrower maintainer instruction and regenerate.`;
  }
  if (request.failure?.detail === "no_repository_change") {
    return "The coding agent completed, but no repository change was generated. If the remedy belongs in this repository, revise the maintainer instruction and regenerate. If it is external or operational, no patch can be generated.";
  }
  return `${detail} Regenerate only if a different instruction could produce a reviewable repository change.`;
}
