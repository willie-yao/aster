import type {
  AnalysisChatProgress,
  AnalysisChatReference,
  AnalysisChatSession,
} from "../types/analysisChat";

const API_BASE = import.meta.env?.BASE_URL ?? "/";
const maxQuestionBytes = 4096;
const utf8Encoder = new TextEncoder();

export const analysisChatActiveTurnLimitMessage = "analysis chat active turn limit reached";
export const analysisChatIdempotencyConflictMessage = "analysis chat idempotency key conflict";
export const analysisChatRateLimitMessage = "analysis chat rate limit reached";
export const analysisChatRequestOutcomeUnknownMessage = "analysis chat request outcome unknown";
export const analysisChatRequestPendingMessage = "analysis chat request is pending";
export const analysisChatSessionBusyMessage = "analysis chat session is busy";
export const analysisChatTurnLimitMessage = "analysis chat turn limit reached";

export class AnalysisChatAPIError extends Error {
  readonly status: number;
  readonly outcome: string | null;

  constructor(status: number, message: string, outcome: string | null = null) {
    super(message);
    this.name = "AnalysisChatAPIError";
    this.status = status;
    this.outcome = outcome;
  }
}

export function isAmbiguousAnalysisChatFailure(error: unknown): boolean {
  return !(error instanceof AnalysisChatAPIError) ||
    (error.status >= 500 && error.outcome === null);
}

export function newAnalysisChatRequestID(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function limitAnalysisChatQuestion(value: string): string {
  let bytes = 0;
  let end = 0;
  for (const character of value) {
    const characterBytes = utf8Encoder.encode(character).byteLength;
    if (bytes + characterBytes > maxQuestionBytes) break;
    bytes += characterBytes;
    end += character.length;
  }
  return value.slice(0, end);
}

async function apiError(response: Response): Promise<AnalysisChatAPIError> {
  const body = (await response.text()).trim();
  return new AnalysisChatAPIError(
    response.status,
    body || `Analysis chat request failed with HTTP ${response.status}`,
    response.headers.get("X-Analysis-Chat-Outcome"),
  );
}

async function parseResponse(response: Response): Promise<AnalysisChatSession> {
  if (!response.ok) throw await apiError(response);
  return response.json() as Promise<AnalysisChatSession>;
}

export async function createAnalysisChatSession(
  analysis: AnalysisChatReference,
  requestID: string,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(`${API_BASE}api/analysis-chat/sessions`, {
    method: "POST",
    credentials: "same-origin",
    cache: "no-store",
    signal,
    headers: { "Content-Type": "application/json", "Idempotency-Key": requestID },
    body: JSON.stringify(analysis),
  });
  return parseResponse(response);
}

export async function findAnalysisChatSession(
  analysis: AnalysisChatReference,
  signal?: AbortSignal,
): Promise<AnalysisChatSession | null> {
  const response = await fetch(`${API_BASE}api/analysis-chat/sessions/lookup`, {
    method: "POST",
    credentials: "same-origin",
    cache: "no-store",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(analysis),
  });
  if (response.status === 204) return null;
  return parseResponse(response);
}

export async function getAnalysisChatSession(
  sessionID: string,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}`,
    {
      credentials: "same-origin",
      cache: "no-store",
      signal,
    },
  );
  return parseResponse(response);
}

export async function sendAnalysisChatMessage(
  sessionID: string,
  message: string,
  requestID: string,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/messages`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json", "Idempotency-Key": requestID },
      body: JSON.stringify({ message }),
    },
  );
  return parseResponse(response);
}


interface AnalysisChatStreamError {
  status: number;
  message: string;
  outcome?: string;
}

export async function streamAnalysisChatMessage(
  sessionID: string,
  message: string,
  requestID: string,
  onProgress: (progress: AnalysisChatProgress) => void,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  let lastError: unknown;
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      return await streamAnalysisChatMessageOnce(sessionID, message, requestID, onProgress, signal);
    } catch (error) {
      if (error instanceof Error && error.name === "AbortError") throw error;
      if (error instanceof AnalysisChatAPIError && !isAmbiguousAnalysisChatFailure(error)) {
        throw error;
      }
      lastError = error;
      if (attempt < 2) await reconnectDelay(400 * (attempt + 1), signal);
    }
  }
  throw lastError instanceof Error ? lastError : new Error("Analysis chat stream disconnected");
}

export async function resumeAnalysisChatTurn(
  session: AnalysisChatSession,
  onProgress: (progress: AnalysisChatProgress) => void,
  signal?: AbortSignal,
  pollDelayMs = 1000,
): Promise<AnalysisChatSession> {
  const active = session.active;
  if (!active) return session;
  onProgress(active);
  if (active.question?.trim()) {
    return streamAnalysisChatMessage(session.id, active.question, active.request_id, onProgress, signal);
  }

  let current = session;
  while (current.active?.request_id === active.request_id) {
    await reconnectDelay(pollDelayMs, signal);
    current = await getAnalysisChatSession(session.id, signal);
    if (current.active?.request_id === active.request_id) onProgress(current.active);
  }
  return current;
}

async function streamAnalysisChatMessageOnce(
  sessionID: string,
  message: string,
  requestID: string,
  onProgress: (progress: AnalysisChatProgress) => void,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/messages/stream`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json", "Idempotency-Key": requestID },
      body: JSON.stringify({ message }),
    },
  );
  if (!response.ok) throw await apiError(response);
  if (!response.body) throw new Error("Analysis chat stream has no response body");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let session: AnalysisChatSession | null = null;
  for (;;) {
    const { done, value } = await reader.read();
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
        if (isAnalysisChatProgress(progress)) onProgress(progress);
      } else if (parsed.event === "session") {
        session = JSON.parse(parsed.data) as AnalysisChatSession;
      } else if (parsed.event === "error") {
        const payload = JSON.parse(parsed.data) as AnalysisChatStreamError;
        throw new AnalysisChatAPIError(payload.status, payload.message, payload.outcome ?? null);
      }
    }
    if (done) break;
  }
  if (!session) throw new Error("Analysis chat stream ended before returning the session");
  return session;
}

function isAnalysisChatProgress(value: unknown): value is AnalysisChatProgress {
  if (!value || typeof value !== "object") return false;
  const candidate = value as Partial<AnalysisChatProgress>;
  return typeof candidate.request_id === "string" &&
    typeof candidate.updated_at === "string" &&
    ["queued", "investigating", "reading_evidence", "evaluating", "finalizing", "cancelling"].includes(
      candidate.phase ?? "",
    );
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
      globalThis.clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    };
    const timer = globalThis.setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, milliseconds);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export async function cancelAnalysisChatRequest(
  sessionID: string,
  requestID: string,
  signal?: AbortSignal,
): Promise<void> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/requests/${encodeURIComponent(requestID)}/cancel`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
    },
  );
  if (!response.ok) throw await apiError(response);
}
