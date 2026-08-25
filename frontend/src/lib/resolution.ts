import { actionErrorMessage } from "./actionRequests";

const API_BASE = import.meta.env.BASE_URL;

// Resolution has two scopes. A pattern resolution acknowledges every cause of a
// pattern at once; a cause resolution acknowledges one cause and leaves its
// siblings visible. The server keys causes by causal-group signature, so id is
// the pattern id for "pattern" and the signature for "cause".
export type ResolutionScope = "pattern" | "cause";

function endpoint(scope: ResolutionScope, id: string, verb: "resolve" | "unresolve"): string {
  const collection = scope === "cause" ? "causes" : "failures";
  return `${API_BASE}api/${collection}/${encodeURIComponent(id)}/${verb}`;
}

async function post(url: string, body?: unknown): Promise<void> {
  const response = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    ...(body === undefined
      ? {}
      : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }),
  });
  if (!response.ok) throw new Error(await actionErrorMessage(response));
}

// resolveFailure hides a failure from the active view until a build newer than
// its current watermark fails the same way.
export async function resolveFailure(
  scope: ResolutionScope,
  id: string,
  note: string,
): Promise<void> {
  await post(endpoint(scope, id, "resolve"), { note: note.trim() });
}

// reopenFailure clears a resolution so the failure returns to the active view.
export async function reopenFailure(scope: ResolutionScope, id: string): Promise<void> {
  await post(endpoint(scope, id, "unresolve"));
}
