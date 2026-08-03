import type { ActionPreview } from "../types/actions";
import { actionErrorMessage } from "./actionRequests";

const API_BASE = import.meta.env.BASE_URL;
const maxInstructionBytes = 4096;
const utf8Encoder = new TextEncoder();

export interface ChatFixPreview extends ActionPreview {
  token: string;
}

export function chatFixInstructionBytes(value: string): number {
  return utf8Encoder.encode(value).byteLength;
}

export function limitChatFixInstruction(value: string): string {
  let bytes = 0;
  let end = 0;
  for (const character of value) {
    const size = utf8Encoder.encode(character).byteLength;
    if (bytes + size > maxInstructionBytes) break;
    bytes += size;
    end += character.length;
  }
  return value.slice(0, end);
}

export async function previewChatFix(
  sessionID: string,
  chatRequestID: string,
  patternID: string,
  patternHash: string,
  sourceRequestID: string | null,
  instruction: string,
  signal?: AbortSignal,
): Promise<ChatFixPreview> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/requests/${encodeURIComponent(chatRequestID)}/fix/preview`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        pattern_id: patternID,
        pattern_hash: patternHash,
        ...(sourceRequestID ? { source_request_id: sourceRequestID } : {}),
        ...(instruction.trim() ? { instruction: instruction.trim() } : {}),
      }),
    },
  );
  if (!response.ok) throw new Error(await actionErrorMessage(response));
  return response.json() as Promise<ChatFixPreview>;
}

export async function confirmChatFix(token: string, signal?: AbortSignal): Promise<string> {
  const response = await fetch(`${API_BASE}api/actions/confirm`, {
    method: "POST",
    credentials: "same-origin",
    cache: "no-store",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
  if (!response.ok) throw new Error(await actionErrorMessage(response));
  const body = (await response.json()) as { url: string };
  return body.url;
}
