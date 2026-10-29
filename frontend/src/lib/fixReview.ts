// Parsers behind the fix review: the unified diff a coding agent returns and the
// Markdown pull request body it drafts. Both are display aids only; the server
// keeps the canonical diff and body.

export interface DiffLine {
  kind: "add" | "del" | "ctx";
  oldNo?: number;
  newNo?: number;
  text: string;
  // The line ends the file without a trailing newline.
  noNewline?: boolean;
}

export interface DiffHunk {
  header: string;
  lines: DiffLine[];
}

export interface DiffFile {
  path: string;
  additions: number;
  deletions: number;
  hunks: DiffHunk[];
}

const hunkHeader = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

// parseUnifiedDiff splits a git diff into files and hunks with old and new
// line numbers. File headers are read only before a file's first hunk, so a
// removed line that starts with "--" stays a removal.
export function parseUnifiedDiff(diff: string): DiffFile[] {
  const files: DiffFile[] = [];
  let file: DiffFile | null = null;
  let hunk: DiffHunk | null = null;
  let oldNo = 0;
  let newNo = 0;
  for (const raw of diff.replace(/\n$/, "").split("\n")) {
    if (raw.startsWith("diff --git ")) {
      file = { path: / b\/(.+)$/.exec(raw)?.[1] ?? raw.slice("diff --git ".length), additions: 0, deletions: 0, hunks: [] };
      files.push(file);
      hunk = null;
      continue;
    }
    if (!hunk && /^(--- |\+\+\+ |index |new file|deleted file|similarity |rename )/.test(raw)) {
      if (file && raw.startsWith("+++ ") && raw !== "+++ /dev/null") file.path = raw.slice(4).replace(/^b\//, "");
      continue;
    }
    const header = hunkHeader.exec(raw);
    if (header) {
      if (!file) {
        file = { path: "", additions: 0, deletions: 0, hunks: [] };
        files.push(file);
      }
      oldNo = Number(header[1]);
      newNo = Number(header[2]);
      hunk = { header: raw, lines: [] };
      file.hunks.push(hunk);
      continue;
    }
    if (!file || !hunk) continue;
    // "\ No newline at end of file" belongs to the line before it. It is the
    // only difference between adding and removing a final newline.
    if (raw.startsWith("\\")) {
      const previous = hunk.lines[hunk.lines.length - 1];
      if (previous) previous.noNewline = true;
      continue;
    }
    if (raw.startsWith("+")) {
      hunk.lines.push({ kind: "add", newNo: newNo++, text: raw.slice(1) });
      file.additions++;
    } else if (raw.startsWith("-")) {
      hunk.lines.push({ kind: "del", oldNo: oldNo++, text: raw.slice(1) });
      file.deletions++;
    } else {
      hunk.lines.push({ kind: "ctx", oldNo: oldNo++, newNo: newNo++, text: raw.startsWith(" ") ? raw.slice(1) : raw });
    }
  }
  return files;
}

export function diffTotals(files: DiffFile[]): { additions: number; deletions: number } {
  return files.reduce(
    (sum, file) => ({ additions: sum.additions + file.additions, deletions: sum.deletions + file.deletions }),
    { additions: 0, deletions: 0 },
  );
}

export type MarkdownBlock =
  | { type: "heading"; level: number; text: string }
  | { type: "paragraph"; text: string }
  | { type: "list"; items: string[] }
  | { type: "quote"; text: string }
  | { type: "code"; text: string };

const listItem = /^\s*[-*]\s+/;

// parseMarkdownBlocks reads the block structure drafted pull request bodies
// use: headings, paragraphs, bullet lists, quotes, and fenced code. HTML
// comments, which pull request templates use for author guidance, are dropped.
export function parseMarkdownBlocks(source: string): MarkdownBlock[] {
  const blocks: MarkdownBlock[] = [];
  const lines = stripMarkdownComments(source).split("\n");
  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (!line.trim()) {
      index++;
      continue;
    }
    if (line.startsWith("```")) {
      const code: string[] = [];
      index++;
      while (index < lines.length && !lines[index].startsWith("```")) code.push(lines[index++]);
      index++;
      blocks.push({ type: "code", text: code.join("\n") });
      continue;
    }
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      blocks.push({ type: "heading", level: heading[1].length, text: heading[2] });
      index++;
      continue;
    }
    if (listItem.test(line)) {
      const items: string[] = [];
      while (index < lines.length && listItem.test(lines[index])) items.push(lines[index++].replace(listItem, ""));
      blocks.push({ type: "list", items });
      continue;
    }
    if (line.startsWith(">")) {
      const quote: string[] = [];
      while (index < lines.length && lines[index].startsWith(">")) quote.push(lines[index++].replace(/^>\s?/, ""));
      blocks.push({ type: "quote", text: quote.join("\n") });
      continue;
    }
    const paragraph: string[] = [];
    while (index < lines.length && lines[index].trim() && !/^(#{1,6}\s|```|>)/.test(lines[index]) && !listItem.test(lines[index])) {
      paragraph.push(lines[index++].trim());
    }
    // GitHub shows a single newline in a pull request body as a line break,
    // and drafted descriptions put each labeled field on its own line.
    blocks.push({ type: "paragraph", text: paragraph.join("\n") });
  }
  return blocks;
}

export function stripMarkdownComments(source: string): string {
  return source.replace(/<!--[\s\S]*?-->/g, "").trim();
}

// A body this short reads in full without crowding the diff below it.
export const shortDescriptionLength = 480;
