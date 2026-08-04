import type {
  AnalysisChatAttempt,
  AnalysisChatMessage,
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
export const analysisChatProviderFailureMessage = "analysis chat provider request failed";
export const analysisChatResponseValidationMessage = "analysis chat model response could not be validated";
export const analysisChatCitationValidationMessage = "analysis chat evidence citation validation failed";

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

export function analysisChatFailureGuidance(error: unknown): string | null {
  if (!(error instanceof AnalysisChatAPIError)) return null;
  switch (error.message) {
    case analysisChatProviderFailureMessage:
      return "The model provider could not complete the request. Try again in a moment.";
    case analysisChatResponseValidationMessage:
      return "The model response could not be validated. Try a narrower question.";
    case analysisChatCitationValidationMessage:
      return "The response's evidence citations could not be validated. Try a narrower evidence question.";
    default:
      return null;
  }
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

export function analysisChatTurnUsage(
  session: AnalysisChatSession,
): { used: number; max: number } | null {
  if (!Number.isInteger(session.turns_used) || session.turns_used < 0) return null;
  if (!Number.isInteger(session.max_turns) || session.max_turns <= 0) return null;
  return { used: session.turns_used, max: session.max_turns };
}

export function analysisChatProgressTurnUsage(
  progress: AnalysisChatProgress,
): { used: number; max: number } | null {
  if (!Number.isInteger(progress.turns_used) || (progress.turns_used ?? -1) < 0) return null;
  if (!Number.isInteger(progress.max_turns) || (progress.max_turns ?? 0) <= 0) return null;
  return { used: progress.turns_used!, max: progress.max_turns! };
}

export function markAnalysisChatTurnLimitReached(
  session: AnalysisChatSession,
): AnalysisChatSession {
  const usage = analysisChatTurnUsage(session);
  if (!usage || usage.used >= usage.max) return session;
  return { ...session, turns_used: usage.max };
}

export function analysisChatTurnLimitReached(
  session: AnalysisChatSession | null,
  hasPendingRequest: boolean,
  rejected: boolean,
): boolean {
  if (hasPendingRequest) return false;
  const usage = session ? analysisChatTurnUsage(session) : null;
  return rejected || usage !== null && usage.used >= usage.max;
}

export type AnalysisChatHistoryEntry =
  | { kind: "message"; key: string; message: AnalysisChatMessage }
  | { kind: "attempt"; key: string; attempt: AnalysisChatAttempt };

export function analysisChatHistory(session: AnalysisChatSession): AnalysisChatHistoryEntry[] {
  const attempts = session.attempts ?? [];
  const attemptByRequest = new Map(attempts.map((attempt) => [attempt.request_id, attempt]));
  const representedRequests = new Set(
    session.messages.flatMap((message) => message.request_id ? [message.request_id] : []),
  );
  const entries: Array<{ entry: AnalysisChatHistoryEntry; order: number; turn: number; time: string }> = [];
  session.messages.forEach((message, index) => {
    entries.push({
      entry: {
        kind: "message",
        key: `message:${message.request_id ?? "legacy"}:${index}`,
        message,
      },
      order: index,
      turn: message.request_id ? attemptByRequest.get(message.request_id)?.turn ?? 0 : 0,
      time: message.created_at,
    });
  });
  attempts.forEach((attempt, index) => {
    if (attempt.outcome === "succeeded" && representedRequests.has(attempt.request_id)) return;
    entries.push({
      entry: {
        kind: "attempt",
        key: `attempt:${attempt.request_id}`,
        attempt,
      },
      order: session.messages.length + index,
      turn: attempt.turn ?? 0,
      time: attempt.created_at ?? attempt.updated_at ?? "",
    });
  });
  entries.sort((left, right) => {
    const leftHasTurn = left.turn > 0;
    const rightHasTurn = right.turn > 0;
    if (leftHasTurn !== rightHasTurn) return leftHasTurn ? -1 : 1;
    if (leftHasTurn && left.turn !== right.turn) return left.turn - right.turn;
    const leftHasTime = left.time !== "";
    const rightHasTime = right.time !== "";
    if (leftHasTime !== rightHasTime) return leftHasTime ? -1 : 1;
    if (left.time !== right.time) return left.time.localeCompare(right.time);
    return left.order - right.order;
  });
  return entries.map(({ entry }) => entry);
}

export function analysisChatAttemptStatus(attempt: AnalysisChatAttempt): { label: string; detail: string } {
  switch (attempt.outcome) {
    case "pending":
      return { label: "Request pending", detail: "The analysis agent is still working on this question." };
    case "succeeded":
      return { label: "Request completed", detail: "The request completed, but its answer is unavailable." };
    case "cancelled":
      return { label: "Request cancelled", detail: "This question was cancelled before an answer was published." };
    case "timed_out":
      return { label: "Request timed out", detail: "The analysis agent timed out before it could answer." };
    case "unknown":
      return { label: "Outcome unknown", detail: "The server could not confirm whether this request completed." };
    case "failed":
      switch (attempt.failure_kind) {
        case "provider":
          return { label: "Provider request failed", detail: "The model provider could not complete this request." };
        case "validation":
          return { label: "Response validation failed", detail: "The model response did not pass validation." };
        case "citation":
          return { label: "Evidence citation validation failed", detail: "The response citations did not pass validation." };
        case "source":
          return { label: "Source investigation failed", detail: "The source investigation could not complete this request." };
        default:
          return { label: "Request failed", detail: "The analysis agent could not complete this request." };
      }
  }
}

export type AnalysisChatRequestState = "answered" | "succeeded" | "terminal" | "pending" | "unresolved";

export function analysisChatRequestState(session: AnalysisChatSession, requestID: string): AnalysisChatRequestState {
  if (session.messages.some((message) => message.request_id === requestID)) return "answered";
  const attempt = session.attempts?.find((candidate) => candidate.request_id === requestID);
  if (attempt?.outcome === "succeeded") return "succeeded";
  if (attempt && attempt.outcome !== "pending") return "terminal";
  if (attempt?.outcome === "pending" || session.active?.request_id === requestID) return "pending";
  return "unresolved";
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
    ["queued", "investigating", "reading_evidence", "evaluating", "finalizing", "validation_retrying", "cancelling"].includes(
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
