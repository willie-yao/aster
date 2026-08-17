import type { AuthStatus } from "../hooks/useAuth";
import type { AnalysisChatMessage, AnalysisChatReference } from "../types/analysisChat";

export interface ChatFixSourceRepository {
  owner: string;
  name: string;
  revision: string;
}

export function chatFixVerifiedSourcePaths(
  fileLinks: Record<string, string> | undefined,
  repository: ChatFixSourceRepository | undefined,
): string[] {
  if (!fileLinks || !repository) return [];
  const owner = repository.owner.trim().toLowerCase();
  const name = repository.name.trim().toLowerCase();
  const revision = repository.revision.trim().toLowerCase();
  if (!owner || !name || !/^[0-9a-f]{40}([0-9a-f]{24})?$/.test(revision)) return [];
  const paths: string[] = [];
  for (const [file, raw] of Object.entries(fileLinks)) {
    try {
      const url = new URL(raw);
      const parts = url.pathname.split("/").filter(Boolean).map((part) => decodeURIComponent(part));
      if (
        url.protocol !== "https:" ||
        url.hostname.toLowerCase() !== "github.com" ||
        parts.length < 5 ||
        parts[0].toLowerCase() !== owner ||
        parts[1].toLowerCase() !== name ||
        parts[2] !== "blob" ||
        parts[3].toLowerCase() !== revision ||
        parts.slice(4).join("/") !== file
      ) {
        continue;
      }
      paths.push(file);
    } catch {
      continue;
    }
  }
  return paths.sort();
}

// chatFixGroundedRequestIDs returns the requests whose answer may start a fix
// preview because the conversation validated an artifact citation on that turn
// or an earlier one. It mirrors the conversation-scoped server gate.
export function chatFixGroundedRequestIDs(messages: AnalysisChatMessage[] | undefined): Set<string> {
  const grounded = new Set<string>();
  let cited = false;
  for (const message of messages ?? []) {
    if (message.role !== "assistant") continue;
    if (message.citations?.length) cited = true;
    if (cited && message.request_id) grounded.add(message.request_id);
  }
  return grounded;
}

export function fixInvestigationAvailable(
  analysis: AnalysisChatReference,
  analysisChatEnabled: boolean,
  junitChatFixEnabled: boolean,
  authStatus: AuthStatus,
  analysisEligible: boolean,
): boolean {
  return analysisEligible && analysisChatEnabled && junitChatFixEnabled &&
    (authStatus === "authenticated" || authStatus === "anonymous") &&
    analysis.scope !== "pattern" && analysis.source !== "build" && Boolean(analysis.junit_file);
}
