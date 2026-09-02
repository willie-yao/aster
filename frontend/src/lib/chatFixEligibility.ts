import type { AnalysisChatMessage } from "../types/analysisChat";

export interface ChatFixSourceRepository {
  owner: string;
  name: string;
  revision: string;
}

// cleanRepositoryPath mirrors Go's path.Clean plus the traversal guards
// buildsource.VerifiedPaths applies. It returns null for a path the server
// would reject.
function cleanRepositoryPath(value: string): string | null {
  if (value.startsWith("/") || value.includes("\\")) return null;
  const segments: string[] = [];
  for (const segment of value.split("/")) {
    if (segment === "" || segment === ".") continue;
    if (segment === "..") {
      if (segments.length === 0) return null;
      segments.pop();
      continue;
    }
    segments.push(segment);
  }
  return segments.length === 0 ? null : segments.join("/");
}

// chatFixVerifiedSourcePaths applies the backend buildsource.VerifiedPaths rule
// to the canonical blob links the server generates: the repository-local path
// comes from the URL, not from the file-link key, because the key keeps the
// path as the analysis cited it. URL parsing here normalizes more than Go does,
// so a malformed link can only make eligibility look available; the server
// preflight still rejects it.
export function chatFixVerifiedSourcePaths(
  fileLinks: Record<string, string> | undefined,
  repository: ChatFixSourceRepository | undefined,
): string[] {
  if (!fileLinks || !repository) return [];
  const owner = repository.owner.trim().toLowerCase();
  const name = repository.name.trim().toLowerCase();
  const revision = repository.revision.trim().toLowerCase();
  if (!owner || !name || !/^[0-9a-f]{40}([0-9a-f]{24})?$/.test(revision)) return [];
  const paths = new Set<string>();
  for (const raw of Object.values(fileLinks)) {
    try {
      const url = new URL(raw.trim());
      const parts = url.pathname.split("/").filter(Boolean);
      if (
        url.protocol !== "https:" ||
        url.hostname.toLowerCase() !== "github.com" ||
        parts.length < 5 ||
        decodeURIComponent(parts[0]).toLowerCase() !== owner ||
        decodeURIComponent(parts[1]).toLowerCase() !== name ||
        parts[2] !== "blob" ||
        decodeURIComponent(parts[3]).toLowerCase() !== revision
      ) {
        continue;
      }
      const clean = cleanRepositoryPath(decodeURIComponent(parts.slice(4).join("/")));
      if (clean) paths.add(clean);
    } catch {
      continue;
    }
  }
  return [...paths].sort();
}

// chatFixVerifiedCitationRequestIDs returns the requests whose answer may start a fix
// preview because the conversation validated an artifact citation on that turn
// or an earlier one. It mirrors the conversation-scoped server gate.
export function chatFixVerifiedCitationRequestIDs(messages: AnalysisChatMessage[] | undefined): Set<string> {
  const verified = new Set<string>();
  let cited = false;
  for (const message of messages ?? []) {
    if (message.role !== "assistant") continue;
    if (message.citations?.some((citation) => !citation.repository && !citation.revision)) cited = true;
    if (cited && message.request_id) verified.add(message.request_id);
  }
  return verified;
}
