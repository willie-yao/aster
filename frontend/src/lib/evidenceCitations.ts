import type { EvidenceCitation } from "../types/dashboard";

// MAX_RENDERED_CITATIONS bounds what one analysis can put on screen. The engine
// already caps its published list, but the UI does not depend on that.
export const MAX_RENDERED_CITATIONS = 20;

// COLLAPSE_THRESHOLD is how many citations stay open before the rest are
// hidden behind a toggle, so a long list cannot bury the analysis below it.
export const COLLAPSE_THRESHOLD = 2;

// formatCitationRange renders the cited lines. A single line reads "L12"
// rather than "L12-L12".
export function formatCitationRange(citation: EvidenceCitation): string {
  if (citation.line_end <= citation.line_start) return `L${citation.line_start}`;
  return `L${citation.line_start}-L${citation.line_end}`;
}

// citationKey identifies one cited region for deduping and for React keys.
export function citationKey(citation: EvidenceCitation): string {
  return `${citation.path}:${citation.line_start}-${citation.line_end}`;
}

// usableCitations returns the citations worth rendering: well-formed, deduped
// by cited region, ordered by path then position, and bounded. Anything
// malformed is dropped rather than rendered as a broken claim.
export function usableCitations(
  citations: EvidenceCitation[] | undefined,
): EvidenceCitation[] {
  if (!citations || citations.length === 0) return [];
  const seen = new Set<string>();
  const usable: EvidenceCitation[] = [];
  for (const citation of citations) {
    const normalized = normalizeCitation(citation);
    if (!normalized) continue;
    const key = citationKey(normalized);
    if (seen.has(key)) continue;
    seen.add(key);
    usable.push(normalized);
  }
  usable.sort(
    (left, right) =>
      left.path.localeCompare(right.path) ||
      left.line_start - right.line_start ||
      left.line_end - right.line_end,
  );
  return usable.slice(0, MAX_RENDERED_CITATIONS);
}

// normalizeCitation cleans one citation and reports it unusable as null.
// Cleaning runs before the emptiness checks because a quote of nothing but
// control sequences is not evidence once those are stripped.
function normalizeCitation(citation: EvidenceCitation | null | undefined): EvidenceCitation | null {
  if (!citation) return null;
  if (typeof citation.path !== "string" || typeof citation.quote !== "string") return null;
  if (!Number.isSafeInteger(citation.line_start) || !Number.isSafeInteger(citation.line_end)) return null;
  if (citation.line_start < 1 || citation.line_end < citation.line_start) return null;
  // Mirrors the engine's own bound. A wider span claims a precision about the
  // evidence that was never established.
  if (citation.line_end - citation.line_start >= MAX_CITED_SPAN) return null;

  const path = citation.path.trim();
  const quote = renderableQuote(citation.quote);
  if (path === "" || quote.trim() === "") return null;

  return { path, line_start: citation.line_start, line_end: citation.line_end, quote };
}

// MAX_CITED_SPAN mirrors the engine's cap on how many lines one citation may
// claim.
const MAX_CITED_SPAN = 200;

// CSI_PATTERN matches colour and cursor sequences. The parameter range is the
// full ECMA-48 set, so colon-separated truecolor codes like ESC[38:2::255:0:0m
// are consumed rather than leaking their tail as visible text.
// eslint-disable-next-line no-control-regex
const CSI_PATTERN = /\u001B\[[0-?]*[ -/]*[@-~]/g;
// OSC_PATTERN matches the hyperlink and title sequences newer CI tooling emits,
// which would otherwise leave printable junk like "]8;;https://..." in the
// quote. It accepts BEL, ST, or end-of-string as the terminator.
// eslint-disable-next-line no-control-regex
const OSC_PATTERN = /\u001B\][^\u0007\u001B]*(?:\u0007|\u001B\\|$)/g;
// eslint-disable-next-line no-control-regex
const LONE_ESCAPE_PATTERN = /\u001B/g;

// renderableQuote turns raw log bytes into what a terminal would have shown.
// Carriage returns matter most: a progress bar emits many per line, and under
// pre-wrap each one would render as another line break and grow the panel
// without bound. Applying overwrite semantics keeps the visible text and the
// height honest.
function renderableQuote(raw: string): string {
  const stripped = raw
    .replace(OSC_PATTERN, "")
    .replace(CSI_PATTERN, "")
    .replace(LONE_ESCAPE_PATTERN, "");
  return stripped
    .replace(/\r\n/g, "\n")
    .split("\n")
    .map((line) => (line.includes("\r") ? line.slice(line.lastIndexOf("\r") + 1) : line))
    .join("\n")
    .replace(/\s+$/, "");
}

// citationSummary labels the section, e.g. "3 citations from 2 artifacts".
export function citationSummary(citations: EvidenceCitation[]): string {
  const artifacts = new Set(citations.map((citation) => citation.path)).size;
  const citationLabel = citations.length === 1 ? "citation" : "citations";
  const artifactLabel = artifacts === 1 ? "artifact" : "artifacts";
  return `${citations.length} ${citationLabel} from ${artifacts} ${artifactLabel}`;
}
