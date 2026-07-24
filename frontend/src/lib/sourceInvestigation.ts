import {
  AnalysisChatAPIError,
  isAmbiguousAnalysisChatFailure,
} from "./analysisChat";
import type {
  SourceInvestigationProgress,
  SourceInvestigationView,
} from "../types/sourceInvestigation";

const API_BASE = import.meta.env.BASE_URL;

export const sourceInvestigationActiveLimitMessage = "source investigation active limit reached";
export const sourceInvestigationIdempotencyConflictMessage = "source investigation idempotency key conflict";
export const sourceInvestigationLimitMessage = "source investigation session limit reached";
export const sourceInvestigationNotFoundMessage = "source investigation not found";
export const sourceInvestigationOutcomeUnknownMessage = "source investigation outcome unknown";
export const sourceInvestigationPendingMessage = "source investigation is pending";

interface SourceInvestigationStreamError {
  status: number;
  message: string;
  outcome?: string;
}

async function sourceAPIError(response: Response): Promise<AnalysisChatAPIError> {
  const body = (await response.text()).trim();
  return new AnalysisChatAPIError(
    response.status,
    body || `Source investigation request failed with HTTP ${response.status}`,
    response.headers.get("X-Analysis-Chat-Outcome"),
  );
}

async function parseSourceResponse(response: Response): Promise<SourceInvestigationView> {
  if (!response.ok) throw await sourceAPIError(response);
  return response.json() as Promise<SourceInvestigationView>;
}

export async function getSourceInvestigation(
  sessionID: string,
  requestID: string,
  signal?: AbortSignal,
): Promise<SourceInvestigationView> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/source-investigations/${encodeURIComponent(requestID)}`,
    {
      credentials: "same-origin",
      cache: "no-store",
      signal,
    },
  );
  return parseSourceResponse(response);
}

export async function streamSourceInvestigation(
  sessionID: string,
  chatRequestID: string,
  requestID: string,
  onProgress: (progress: SourceInvestigationProgress) => void,
  signal?: AbortSignal,
): Promise<SourceInvestigationView> {
  let lastError: unknown;
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      return await streamSourceInvestigationOnce(
        sessionID,
        chatRequestID,
        requestID,
        onProgress,
        signal,
      );
    } catch (error) {
      if (error instanceof Error && error.name === "AbortError") throw error;
      if (error instanceof AnalysisChatAPIError && !isAmbiguousAnalysisChatFailure(error)) {
        throw error;
      }
      lastError = error;
      if (attempt < 2) await reconnectDelay(400 * (attempt + 1), signal);
    }
  }
  throw lastError instanceof Error ? lastError : new Error("Source investigation stream disconnected");
}

async function streamSourceInvestigationOnce(
  sessionID: string,
  chatRequestID: string,
  requestID: string,
  onProgress: (progress: SourceInvestigationProgress) => void,
  signal?: AbortSignal,
): Promise<SourceInvestigationView> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/source-investigations/stream`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json", "Idempotency-Key": requestID },
      body: JSON.stringify({ chat_request_id: chatRequestID }),
    },
  );
  if (!response.ok) throw await sourceAPIError(response);
  if (!response.body) throw new Error("Source investigation stream has no response body");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let investigation: SourceInvestigationView | null = null;
  for (;;) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    const chunks = buffer.split(/\r?\n\r?\n/);
    buffer = chunks.pop() ?? "";
    if (done && buffer.trim() !== "") {
      chunks.push(buffer);
      buffer = "";
    }
    for (const chunk of chunks) {
      const parsed = parseSSEChunk(chunk);
      if (!parsed) continue;
      if (parsed.event === "progress") {
        const progress = JSON.parse(parsed.data) as unknown;
        if (isSourceInvestigationProgress(progress)) onProgress(progress);
      } else if (parsed.event === "investigation") {
        investigation = JSON.parse(parsed.data) as SourceInvestigationView;
      } else if (parsed.event === "error") {
        const payload = JSON.parse(parsed.data) as SourceInvestigationStreamError;
        throw new AnalysisChatAPIError(payload.status, payload.message, payload.outcome ?? null);
      }
    }
    if (done) break;
  }
  if (!investigation) throw new Error("Source investigation stream ended before returning a result");
  return investigation;
}

export async function cancelSourceInvestigation(
  sessionID: string,
  requestID: string,
  signal?: AbortSignal,
): Promise<void> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/source-investigations/${encodeURIComponent(requestID)}/cancel`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
    },
  );
  if (!response.ok) throw await sourceAPIError(response);
}

function isSourceInvestigationProgress(value: unknown): value is SourceInvestigationProgress {
  if (!value || typeof value !== "object") return false;
  const candidate = value as Partial<SourceInvestigationProgress>;
  return typeof candidate.request_id === "string" &&
    typeof candidate.updated_at === "string" &&
    [
      "queued",
      "cloning_source",
      "investigating_source",
      "verifying_citations",
      "finalizing",
      "cancelling",
    ].includes(candidate.phase ?? "");
}

function parseSSEChunk(chunk: string): { event: string; data: string } | null {
  let event = "message";
  const data: string[] = [];
  for (const line of chunk.split(/\r?\n/)) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
  }
  return data.length > 0 ? { event, data: data.join("\n") } : null;
}

function reconnectDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException("Aborted", "AbortError"));
      return;
    }
    const onAbort = () => {
      window.clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    };
    const timer = window.setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, milliseconds);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
