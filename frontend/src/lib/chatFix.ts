import type { ActionPreview, ActionRequest } from "../types/actions";
import { actionErrorMessage, cancelActionRequest, loadLatestActionRequest } from "./actionRequests.js";

const API_BASE = import.meta.env.BASE_URL;
const maxInstructionBytes = 4096;
const utf8Encoder = new TextEncoder();

export interface ChatFixPreview extends ActionPreview {
  token: string;
}

export interface ChatFixRequest extends Omit<ActionRequest, "kind" | "preview"> {
  kind: "analysis-fix";
  preview?: ChatFixPreview;
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
  patternID: string | null,
  patternHash: string | null,
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
        ...(patternID ? { pattern_id: patternID } : {}),
        ...(patternHash ? { pattern_hash: patternHash } : {}),
        ...(sourceRequestID ? { source_request_id: sourceRequestID } : {}),
        ...(instruction.trim() ? { instruction: instruction.trim() } : {}),
      }),
    },
  );
  if (!response.ok) throw new Error(await actionErrorMessage(response));
  return response.json() as Promise<ChatFixPreview>;
}

export async function createAnalysisChatFixRequest(
  sessionID: string,
  chatRequestID: string,
  instruction: string,
  signal?: AbortSignal,
): Promise<ChatFixRequest> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/requests/${encodeURIComponent(chatRequestID)}/fix/requests`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(instruction.trim() ? { instruction: instruction.trim() } : {}),
    },
  );
  if (!response.ok) throw new Error(await actionErrorMessage(response));
  return validateChatFixRequest(await response.json() as ActionRequest);
}

export async function loadAnalysisChatFixRequest(id: string, signal?: AbortSignal): Promise<ChatFixRequest> {
  return validateChatFixRequest(await loadLatestActionRequest(API_BASE, id, signal));
}


export async function cancelAnalysisChatFixRequest(id: string): Promise<ChatFixRequest> {
  return validateChatFixRequest(await cancelActionRequest(API_BASE, id));
}

function validateChatFixRequest(request: ActionRequest): ChatFixRequest {
  if ((request as { kind: string }).kind !== "analysis-fix") throw new Error("The saved request is not an exact JUnit fix preview.");
  if (request.preview && (!request.preview.token || request.preview.kind !== "fix")) {
    throw new Error("The saved exact JUnit fix preview is incomplete.");
  }
  return request as unknown as ChatFixRequest;
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
