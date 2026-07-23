import type {
  AnalysisChatReference,
  AnalysisChatSession,
} from "../types/analysisChat";

const API_BASE = import.meta.env.BASE_URL;

export class AnalysisChatAPIError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "AnalysisChatAPIError";
    this.status = status;
  }
}

async function parseResponse(response: Response): Promise<AnalysisChatSession> {
  if (!response.ok) {
    const body = (await response.text()).trim();
    throw new AnalysisChatAPIError(
      response.status,
      body || `Analysis chat request failed with HTTP ${response.status}`,
    );
  }
  return response.json() as Promise<AnalysisChatSession>;
}

export async function createAnalysisChatSession(
  analysis: AnalysisChatReference,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(`${API_BASE}api/analysis-chat/sessions`, {
    method: "POST",
    credentials: "same-origin",
    cache: "no-store",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(analysis),
  });
  return parseResponse(response);
}

export async function sendAnalysisChatMessage(
  sessionID: string,
  message: string,
  signal?: AbortSignal,
): Promise<AnalysisChatSession> {
  const response = await fetch(
    `${API_BASE}api/analysis-chat/sessions/${encodeURIComponent(sessionID)}/messages`,
    {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message }),
    },
  );
  return parseResponse(response);
}
