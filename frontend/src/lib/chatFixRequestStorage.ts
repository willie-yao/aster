export interface StoredChatFixRequest {
  id: string;
  instruction: string;
}

type ChatFixRequestStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const maxInstructionBytes = 4096;
const storedRequestIDPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const utf8Encoder = new TextEncoder();

function limitInstruction(value: string): string {
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

export function chatFixRequestStorageKey(sessionID: string, chatRequestID: string): string {
  return [
    "prow-ai-dashboard:chat-fix-request",
    encodeURIComponent(sessionID),
    encodeURIComponent(chatRequestID),
  ].join(":");
}

export function readStoredChatFixRequest(
  storage: ChatFixRequestStorage,
  sessionID: string,
  chatRequestID: string,
): StoredChatFixRequest | null {
  try {
    const raw = storage.getItem(chatFixRequestStorageKey(sessionID, chatRequestID));
    if (!raw) return null;
    const value = JSON.parse(raw) as Partial<StoredChatFixRequest>;
    if (!storedRequestIDPattern.test(value.id ?? "") || typeof value.instruction !== "string") return null;
    return { id: value.id!, instruction: limitInstruction(value.instruction) };
  } catch {
    return null;
  }
}

export function storeChatFixRequest(
  storage: ChatFixRequestStorage,
  sessionID: string,
  chatRequestID: string,
  request: StoredChatFixRequest,
): void {
  if (!storedRequestIDPattern.test(request.id)) return;
  try {
    storage.setItem(
      chatFixRequestStorageKey(sessionID, chatRequestID),
      JSON.stringify({ id: request.id, instruction: limitInstruction(request.instruction) }),
    );
  } catch {
    // The open dialog still retains the request identity when storage is unavailable.
  }
}

export function clearStoredChatFixRequest(
  storage: ChatFixRequestStorage,
  sessionID: string,
  chatRequestID: string,
): void {
  try {
    storage.removeItem(chatFixRequestStorageKey(sessionID, chatRequestID));
  } catch {
    // Storage is optional.
  }
}
