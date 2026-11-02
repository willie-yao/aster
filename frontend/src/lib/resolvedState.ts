import type { ResolvedState } from "../types/dashboard";

export interface ResolvedReadState {
  data: ResolvedState | null;
  loading: boolean;
  error: string | null;
}

export type ResolvedReadEvent =
  | { type: "loading" }
  | { type: "success"; data: ResolvedState }
  | { type: "error"; error: string };

export const initialResolvedRead: ResolvedReadState = { data: null, loading: true, error: null };
export type ResolutionAvailability = "loading" | "available" | "unavailable";

export function resolvedReadState(state: ResolvedReadState, event: ResolvedReadEvent): ResolvedReadState {
  switch (event.type) {
    case "loading": return { ...state, loading: true, error: null };
    case "success": return { data: event.data, loading: false, error: null };
    case "error": return { ...state, loading: false, error: event.error };
  }
}

export function resolutionAvailability(state: ResolvedReadState): ResolutionAvailability {
  if (state.loading) return "loading";
  return state.data !== null && state.error === null ? "available" : "unavailable";
}

function waitForRetry(delay: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, delay);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

export async function readResolvedState({
  url, signal, onResult, fetcher = fetch, wait = waitForRetry,
}: {
  url: string;
  signal: AbortSignal;
  onResult: (event: ResolvedReadEvent) => void;
  fetcher?: typeof fetch;
  wait?: (delay: number, signal: AbortSignal) => Promise<void>;
}): Promise<void> {
  const attempts = 5;
  for (let attempt = 0; attempt < attempts && !signal.aborted; attempt += 1) {
    try {
      const response = await fetcher(url, { cache: "no-store", signal });
      if (signal.aborted) return;
      let data: ResolvedState;
      if (response.status === 404) {
        data = { resolved: {}, causes: {} };
      } else {
        if (!response.ok) throw new Error(`resolved.json: ${response.status}`);
        const body = await response.json() as Partial<ResolvedState> | null;
        if (!body?.resolved || typeof body.resolved !== "object" || Array.isArray(body.resolved) ||
            (body.causes != null && (typeof body.causes !== "object" || Array.isArray(body.causes)))) {
          throw new Error("Invalid resolution state");
        }
        data = { resolved: body.resolved, causes: body.causes ?? {} };
      }
      if (signal.aborted) return;
      onResult({ type: "success", data });
      return;
    } catch (error) {
      if (signal.aborted) return;
      if (attempt === attempts - 1) {
        onResult({ type: "error", error: error instanceof Error ? error.message : String(error) });
        return;
      }
    }
    await wait(Math.min(8000, 500 * 2 ** attempt), signal);
  }
}
