import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import { Fragment, type ReactNode } from "react";
import { formatSteps, fileToUrl, type FileToUrlContext } from "../lib/utils";

interface RichTextProps {
  text: string;
  /**
   * Format non-code segments as multi-line analysis prose by inserting breaks
   * before numbered steps and bullets. Leave false for short summaries.
   */
  steps?: boolean;
  /**
   * Render code spans and bare file paths as links only when they resolve to a
   * known prow artifact or source file. Unresolvable tokens stay plain.
   */
  fileCtx?: FileToUrlContext;
}

const codeSx = {
  fontFamily: "monospace",
  fontSize: "0.85em",
  px: 0.5,
  py: 0.125,
  borderRadius: "4px",
  bgcolor: "action.selected",
  color: "text.primary",
  wordBreak: "break-word",
} as const;

const codeLinkSx = {
  ...codeSx,
  color: "primary.main",
  textDecorationColor: "transparent",
  "&:hover": { textDecorationColor: "inherit" },
} as const;

// Bare paths use monospace link styling without a pill, so dense
// prose with many paths stays readable.
const pathLinkSx = {
  fontFamily: "monospace",
  color: "primary.main",
  wordBreak: "break-word",
  textDecorationColor: "transparent",
  "&:hover": { textDecorationColor: "inherit" },
} as const;

const CODE_SPLIT = /`([^`]+)`/g;

function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i >= 0 ? path.slice(i + 1) : path;
}

// Candidate file path in prose: slash-separated segments ending in a known
// source or artifact extension. Trailing line refs such as :120 or :120-130 are
// excluded so the path still resolves.
const PATH_RE =
  /(?:[\w.-]+\/)+[\w.-]+\.(?:go|ya?ml|sh|json|tpl|md|log|txt|xml|out|conf)\b/g;

// Linkify resolvable bare file paths in prose. Return the raw string when
// nothing resolves so the parent's pre-line whitespace handling stays intact.
function linkifyPaths(
  text: string,
  fileCtx: FileToUrlContext | undefined,
  keyBase: number,
): ReactNode {
  if (!fileCtx) return text;
  PATH_RE.lastIndex = 0;
  const out: ReactNode[] = [];
  let last = 0;
  let k = 0;
  let m: RegExpExecArray | null;
  while ((m = PATH_RE.exec(text)) !== null) {
    const token = m[0];
    const url = fileToUrl(token, fileCtx);
    if (!url) continue;
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(
      <Link
        key={`${keyBase}-${k++}`}
        href={url}
        target="_blank"
        rel="noopener noreferrer"
        sx={pathLinkSx}
        title={token}
      >
        {basename(token)}
      </Link>,
    );
    last = m.index + token.length;
  }
  if (out.length === 0) return text;
  if (last < text.length) out.push(text.slice(last));
  return out;
}

/**
 * Render AI analysis text with markdown-style inline `code` spans as styled
 * code. Code spans and bare paths become links only when fileCtx resolves them
 * to a real prow artifact or source file. Everything else renders verbatim.
 */
export function RichText({ text, steps = false, fileCtx }: RichTextProps) {
  const parts = text.split(CODE_SPLIT);
  return (
    <>
      {parts.map((part, i) => {
        if (i % 2 === 0) {
          const formatted = steps ? formatSteps(part) : part;
          return <Fragment key={i}>{linkifyPaths(formatted, fileCtx, i)}</Fragment>;
        }
        const url = fileCtx ? fileToUrl(part, fileCtx) : null;
        if (url) {
          return (
            <Link
              key={i}
              href={url}
              target="_blank"
              rel="noopener noreferrer"
              sx={codeLinkSx}
              title={part}
            >
              {basename(part)}
            </Link>
          );
        }
        return (
          <Box component="code" key={i} sx={codeSx}>
            {part}
          </Box>
        );
      })}
    </>
  );
}
