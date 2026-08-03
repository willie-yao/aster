export type Action = "create-issue" | "propose-fix";

export type RequestStatus =
  | "pending"
  | "ready"
  | "unknown"
  | "failed"
  | "confirmed"
  | "cancelled"
  | "expired";

export interface ActionPreview {
  kind: "issue" | "fix";
  title: string;
  body: string;
  diff?: string;
  verify_status?: string;
  verify_summary?: string;
  verify_output?: string;
}

export interface ActionRequest {
  id: string;
  failure_id: string;
  kind: Action;
  owner: string;
  status: RequestStatus;
  created_at: string;
  updated_at: string;
  expires_at: string;
  error?: string;
  warning?: string;
  result_url?: string;
  superseded_by?: string;
  preview?: ActionPreview;
  email_sent?: boolean;
  email_error?: string;
}

export async function actionErrorMessage(response: Response): Promise<string> {
  const text = (await response.text()).trim();
  const contentType = response.headers.get("content-type") ?? "";
  if (contentType.includes("text/html") || /^<!doctype\s+html/i.test(text)) {
    return `Request failed with HTTP ${response.status}. The gateway returned an HTML error page.`;
  }
  return text || `HTTP ${response.status}`;
}

export async function loadLatestActionRequest(
  apiBase: string,
  id: string,
): Promise<ActionRequest> {
  const seen = new Set<string>();
  let currentID = id;
  for (;;) {
    if (seen.has(currentID)) {
      throw new Error("Action request replacement cycle detected.");
    }
    seen.add(currentID);
    const response = await fetch(
      `${apiBase}api/action-requests/${encodeURIComponent(currentID)}`,
      { credentials: "same-origin", cache: "no-store" },
    );
    if (!response.ok) throw new Error(await actionErrorMessage(response));
    const request = (await response.json()) as ActionRequest;
    if (!request.superseded_by) return request;
    currentID = request.superseded_by;
  }
}
