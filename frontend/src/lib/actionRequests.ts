import type { Action, ActionRequest, RequestStatus } from "../types/actions";

export type ActionRequestStorage = Pick<
  Storage,
  "getItem" | "setItem" | "removeItem"
>;

const storedRequestIDPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;

export function actionRequestStorageOwner(
  login: string | null,
  mode: "oauth" | "proxy" | "dev" | null,
): string | null {
  const normalizedLogin = login?.trim().toLowerCase();
  if (normalizedLogin) return normalizedLogin;
  return mode ? `mode:${mode}` : null;
}

export function actionRequestStorageKey(
  owner: string,
  failureID: string,
  action: Action,
): string {
  return [
    "prow-ai-dashboard:action-request",
    encodeURIComponent(owner.trim().toLowerCase()),
    encodeURIComponent(failureID),
    encodeURIComponent(action),
  ].join(":");
}

export function readStoredActionRequestID(
  storage: ActionRequestStorage,
  owner: string,
  failureID: string,
  action: Action,
): string | null {
  try {
    const value =
      storage
        .getItem(actionRequestStorageKey(owner, failureID, action))
        ?.trim() ?? "";
    return storedRequestIDPattern.test(value) ? value : null;
  } catch {
    return null;
  }
}

export function storeActionRequestID(
  storage: ActionRequestStorage,
  owner: string,
  failureID: string,
  action: Action,
  requestID: string,
): void {
  if (!storedRequestIDPattern.test(requestID)) return;
  try {
    storage.setItem(
      actionRequestStorageKey(owner, failureID, action),
      requestID,
    );
  } catch {
    // The active component still retains the request ID when storage is unavailable.
  }
}

export function clearStoredActionRequestID(
  storage: ActionRequestStorage,
  owner: string,
  failureID: string,
  action: Action,
): void {
  try {
    storage.removeItem(actionRequestStorageKey(owner, failureID, action));
  } catch {
    // Storage is optional.
  }
}

export function actionRequestIsPollable(status: RequestStatus): boolean {
  return status === "pending" || status === "cancelling";
}

export function actionRequestIsActive(
  request: ActionRequest,
  now = Date.now(),
): boolean {
  if (actionRequestIsPollable(request.status) || request.status === "unknown") {
    return true;
  }
  if (request.status !== "ready") return false;
  const expiresAt = Date.parse(request.expires_at);
  return Number.isFinite(expiresAt) && expiresAt > now;
}

export function actionRequestCanConfirm(
  status: RequestStatus,
  hasPreview: boolean,
): boolean {
  return status === "unknown" || (status === "ready" && hasPreview);
}

export function actionRequestIsTerminal(status: RequestStatus): boolean {
  return (
    status === "failed" ||
    status === "confirmed" ||
    status === "cancelled" ||
    status === "expired"
  );
}

export function actionRequestIsRecoverable(status: RequestStatus): boolean {
  return !actionRequestIsTerminal(status);
}

export function actionRequestProgressTitle(
  request: ActionRequest | null,
  isFix: boolean,
): string {
  if (!request || request.stage !== "drafting") {
    return "Verifying the proposed remediation against pinned source";
  }
  return isFix ? "Generating the fix proposal" : "Preparing the issue draft";
}

export function actionRequestProgressDetail(
  request: ActionRequest | null,
): string {
  if (
    request?.stage === "drafting" &&
    request.verification?.state === "unresolved"
  ) {
    return "The source target is verified as unresolved. Draft generation has started.";
  }
  if (!request || request.stage !== "drafting") {
    return "The dashboard checks the structured target before starting any model or runtime work.";
  }
  return "Generation continues in the background. You can leave this page and return later.";
}

export function actionRequestReasonTitle(request: ActionRequest): string | null {
  switch (request.reason_code) {
    case "recovered": return "Watching recovery";
    case "observing": return "Observing verified remediation";
    case "verified_fixed": return "Verified fixed";
    case "retained_stale": return "Retained analysis cannot start an action";
    case "non_systemic": return "Not a recurring systemic pattern";
    case "evidence_unavailable": return "Current evidence unavailable";
    case "investigation_required": return "Source investigation required";
    case "no_reviewable_patch": return "No reviewable patch generated";
    case "contract_generation_failed": return "Preview generation failed";
    case "unsafe_remediation": return "Unsafe remediation blocked";
    case "already_present": return "Remediation already exists";
    case "source_verification_inconclusive": return "Source verification inconclusive";
    case "generation_failed": return "Draft generation failed";
    default: return null;
  }
}

export function actionRequestVerificationTitle(
  request: ActionRequest,
): string | null {
  const verification = request.verification;
  if (!verification) return null;
  if (verification.code === "unsafe_remediation") return "Unsafe remediation blocked";
  if (verification.code === "investigation_required") return "Source investigation required";
  if (verification.code === "recovered") return "Watching recovery";
  if (verification.code === "observing") return "Observing verified remediation";
  if (verification.code === "verified_fixed") return "Verified fixed";
  if (verification.state === "already_present") {
    const reason = verification.reason.toLowerCase();
    if (reason.startsWith("configuration ")) {
      const absent = reason.includes("already absent");
      return absent ? "Configuration already absent" : "Configuration already applied";
    }
    return "Existing remediation detected";
  }
  if (verification.state === "unresolved")
    return "Verified target is unresolved";
  return "Source verification inconclusive";
}

export function actionRequestVerificationDetail(
  request: ActionRequest,
): string | null {
  const verification = request.verification;
  if (!verification) return null;
  if (verification.state === "already_present") {
    return `${verification.reason}. Check whether the pattern is stale, regressed, or misclassified.`;
  }
  if (verification.state === "inconclusive") {
    return `${verification.reason}. Investigate the pinned source before starting an action.`;
  }
  return verification.reason;
}

export function actionRequestHasBlockingVerification(
  request: ActionRequest,
): boolean {
  return (
    request.status === "failed" &&
    (request.verification?.state === "already_present" ||
      request.verification?.state === "inconclusive")
  );
}

export function syncStoredActionRequest(
  storage: ActionRequestStorage,
  owner: string,
  request: ActionRequest,
): void {
  if (actionRequestIsTerminal(request.status)) {
    clearStoredActionRequestID(
      storage,
      owner,
      request.failure_id,
      request.kind,
    );
    return;
  }
  storeActionRequestID(
    storage,
    owner,
    request.failure_id,
    request.kind,
    request.id,
  );
}

export async function actionErrorMessage(response: Response): Promise<string> {
  const text = (await response.text()).trim();
  const contentType = response.headers.get("content-type") ?? "";
  if (contentType.includes("text/html") || /^<!doctype\s+html/i.test(text)) {
    return `Request failed with HTTP ${response.status}. The gateway returned an HTML error page.`;
  }
  if (contentType.includes("application/json") && text) {
    try {
      const value = JSON.parse(text) as { message?: unknown; error?: unknown };
      if (typeof value.message === "string" && value.message.trim()) return value.message.trim();
      if (typeof value.error === "string" && value.error.trim()) return value.error.trim();
    } catch {
      // Older and intermediary servers may return non-JSON error text.
    }
  }
  return text || `HTTP ${response.status}`;
}

async function requestView(response: Response): Promise<ActionRequest> {
  if (!response.ok) throw new Error(await actionErrorMessage(response));
  return response.json() as Promise<ActionRequest>;
}

export async function loadLatestActionRequest(
  apiBase: string,
  id: string,
  signal?: AbortSignal,
): Promise<ActionRequest> {
  const seen = new Set<string>();
  let currentID = id;
  for (;;) {
    if (seen.has(currentID)) {
      throw new Error("Action request replacement cycle detected.");
    }
    seen.add(currentID);
    const request = await requestView(
      await fetch(
        `${apiBase}api/action-requests/${encodeURIComponent(currentID)}`,
        { credentials: "same-origin", cache: "no-store", signal },
      ),
    );
    if (!request.superseded_by) return request;
    currentID = request.superseded_by;
  }
}

export async function cancelActionRequest(
  apiBase: string,
  id: string,
): Promise<ActionRequest> {
  return requestView(
    await fetch(
      `${apiBase}api/action-requests/${encodeURIComponent(id)}/cancel`,
      { method: "POST", credentials: "same-origin" },
    ),
  );
}
