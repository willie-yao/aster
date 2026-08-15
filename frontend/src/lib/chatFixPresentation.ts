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
  if (request.status === "failed" && request.reason_code === "no_reviewable_patch") {
    return {
      severity: "warning",
      message: request.error || "No reviewable patch was generated. Add a maintainer instruction and regenerate.",
      canRegenerate: true,
      shouldObserve: false,
    };
  }
  if (request.status === "failed") {
    return {
      severity: "error",
      message: hardFailureMessage(request),
      canRegenerate: false,
      shouldObserve: false,
    };
  }
  return null;
}

function hardFailureMessage(request: ChatFixRequest): string {
  switch (request.failure?.category) {
    case "runtime_infrastructure":
      return "Fix preview generation failed in the isolated runtime.";
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
