import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import CircularProgress from "@mui/material/CircularProgress";
import DialogTitle from "@mui/material/DialogTitle";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import Popover from "@mui/material/Popover";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import {
  ArrowBackOutlined,
  BuildOutlined,
  CheckCircleOutlined,
  CloseOutlined,
  ErrorOutlined,
  ExpandMoreOutlined,
} from "@mui/icons-material";
import { accentLabelSx, soft } from "../theme";
import { dialogRegionLabelSx, overlayPaperSx, overviewTypography, sectionBandSx } from "../theme/overview";
import {
  diffTotals,
  parseMarkdownBlocks,
  parseUnifiedDiff,
  shortDescriptionLength,
  stripMarkdownComments,
  type DiffFile,
} from "../lib/fixReview";
import type { ActionPreview } from "../types/actions";

const mono = overviewTypography.data.fontFamily;
const easeOut = "cubic-bezier(0.22, 1, 0.36, 1)";

const focusRingSx = {
  "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 },
} as const;

export interface FixReviewProps {
  preview: ActionPreview;
  branch?: string;
  instruction: string;
  onInstruction: (value: string) => void;
  instructionBytes: number;
  canRevise: boolean;
  reviseDisabled: boolean;
  onRevise: () => void;
  generating: boolean;
  sentInstruction: string;
  busy: "preview" | "regenerate" | "confirm" | null;
  hasWarnings: boolean;
  error: string | null;
  observation: string | null;
  onBack: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}

function ChangeCounts({ additions, deletions }: { additions: number; deletions: number }) {
  return (
    <Box
      component="span"
      aria-label={`${additions} added, ${deletions} removed`}
      sx={{ ...overviewTypography.data, display: "inline-flex", gap: 0.75, whiteSpace: "nowrap" }}
    >
      <Box component="span" sx={(theme) => accentLabelSx(theme, "success")}>+{additions}</Box>
      <Box component="span" sx={(theme) => accentLabelSx(theme, "error")}>−{deletions}</Box>
    </Box>
  );
}

const gutterSx = {
  width: 44,
  flexShrink: 0,
  pr: 1,
  textAlign: "right",
  color: "text.secondary",
  userSelect: "none",
} as const;

/** Unified diff with old and new line numbers and added and removed lines tinted. */
function DiffView({ files }: { files: DiffFile[] }) {
  return (
    <Box sx={{ display: "grid", gap: 1.5 }}>
      {files.map((file) => (
        <Box
          key={file.path}
          data-diff-file={file.path}
          sx={{ border: "1px solid", borderColor: "divider", borderRadius: 1, overflow: "hidden", bgcolor: "surface.containerLow" }}
        >
          <Box
            sx={{
              display: "flex",
              alignItems: "center",
              gap: 1.5,
              px: 1.5,
              minHeight: 40,
              bgcolor: "surface.containerHigh",
              borderBottom: "1px solid",
              borderColor: "divider",
            }}
          >
            <Typography component="h3" sx={{ ...overviewTypography.data, fontWeight: 650, minWidth: 0, flex: 1, overflowWrap: "anywhere" }}>
              {file.path || "Changes"}
            </Typography>
            <ChangeCounts additions={file.additions} deletions={file.deletions} />
          </Box>
          <Box sx={{ overflowX: "auto" }}>
            <Box sx={{ display: "inline-block", minWidth: "100%", py: 0.5, fontFamily: mono, fontSize: "12.5px", lineHeight: "21px" }}>
              {file.hunks.map((hunk, hunkIndex) => (
                <Box
                  key={hunkIndex}
                  data-diff-hunk
                  tabIndex={-1}
                  sx={{ scrollMarginTop: 8, "&:focus": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 } }}
                >
                  <Box sx={(theme) => ({ display: "flex", bgcolor: soft(theme, "primary", 0.08), color: "text.secondary" })}>
                    <Box component="span" sx={{ ...gutterSx, width: 108 }} />
                    <Box component="span" sx={{ whiteSpace: "pre", pr: 2 }}>{hunk.header}</Box>
                  </Box>
                  {hunk.lines.map((line, lineIndex) => [
                    <Box
                      key={lineIndex}
                      sx={(theme) => ({
                        display: "flex",
                        bgcolor: line.kind === "add"
                          ? soft(theme, "success", 0.13)
                          : line.kind === "del" ? soft(theme, "error", 0.13) : "transparent",
                      })}
                    >
                      <Box component="span" sx={gutterSx}>{line.oldNo ?? ""}</Box>
                      <Box component="span" sx={gutterSx}>{line.newNo ?? ""}</Box>
                      <Box
                        component="span"
                        aria-hidden="true"
                        sx={(theme) => ({
                          width: 20,
                          flexShrink: 0,
                          textAlign: "center",
                          userSelect: "none",
                          ...(line.kind === "add"
                            ? accentLabelSx(theme, "success")
                            : line.kind === "del" ? accentLabelSx(theme, "error") : {}),
                        })}
                      >
                        {line.kind === "add" ? "+" : line.kind === "del" ? "−" : ""}
                      </Box>
                      <Box component="span" sx={{ whiteSpace: "pre", pr: 2 }}>{line.text || " "}</Box>
                    </Box>,
                    line.noNewline && (
                      <Box key={`${lineIndex}-eof`} sx={{ display: "flex", color: "text.secondary" }}>
                        <Box component="span" sx={{ ...gutterSx, width: 108 }} />
                        <Box component="span" sx={{ whiteSpace: "pre", pr: 2, fontStyle: "italic" }}>\ No newline at end of file</Box>
                      </Box>
                    ),
                  ])}
                </Box>
              ))}
            </Box>
          </Box>
        </Box>
      ))}
    </Box>
  );
}

function inlineMarkdown(text: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  // Link destinations may be wrapped in angle brackets, which the engine uses.
  const pattern = /(`[^`]+`)|\*\*([^*]+)\*\*|\[([^\]]+)\]\(<?(https?:\/\/[^)\s>]+)>?\)/g;
  let last = 0;
  let match: RegExpExecArray | null;
  while ((match = pattern.exec(text))) {
    if (match.index > last) nodes.push(text.slice(last, match.index));
    if (match[1]) {
      nodes.push(
        <Box
          key={match.index}
          component="code"
          sx={{ fontFamily: mono, fontSize: "0.86em", px: 0.5, borderRadius: 0.5, bgcolor: "surface.containerHigh", overflowWrap: "anywhere" }}
        >
          {match[1].slice(1, -1)}
        </Box>,
      );
    } else if (match[2]) {
      nodes.push(<strong key={match.index}>{match[2]}</strong>);
    } else {
      nodes.push(
        <Link key={match.index} href={match[4]} target="_blank" rel="noopener noreferrer" sx={{ overflowWrap: "anywhere" }}>
          {match[3]}
        </Link>,
      );
    }
    last = match.index + match[0].length;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}

/** The drafted pull request body, rendered the way GitHub will show it. */
function MarkdownBody({ text }: { text: string }) {
  const blocks = useMemo(() => parseMarkdownBlocks(text), [text]);
  if (blocks.length === 0) return <Typography color="textSecondary">No description.</Typography>;
  return (
    <Box sx={{ maxWidth: "72ch", ...overviewTypography.primaryBody, "& > * + *": { mt: 1.25 }, "& > h3 + *, & > h4 + *": { mt: 0.5 } }}>
      {blocks.map((block, index) => {
        switch (block.type) {
          case "heading":
            return (
              <Typography
                key={index}
                component={block.level <= 2 ? "h3" : "h4"}
                sx={{ ...overviewTypography.categoryHeading, fontSize: block.level <= 2 ? "16px" : "14.5px", "&:not(:first-of-type)": { mt: 2.5 } }}
              >
                {inlineMarkdown(block.text)}
              </Typography>
            );
          case "list":
            return (
              <Box key={index} component="ul" sx={{ m: 0, pl: 2.5, "& li + li": { mt: 0.5 } }}>
                {block.items.map((item, itemIndex) => <li key={itemIndex}>{inlineMarkdown(item)}</li>)}
              </Box>
            );
          case "quote":
            return (
              <Box key={index} component="blockquote" sx={{ m: 0, pl: 1.5, borderLeft: "2px solid", borderColor: "divider", color: "text.secondary", whiteSpace: "pre-line" }}>
                {inlineMarkdown(block.text)}
              </Box>
            );
          case "code":
            return (
              <Box
                key={index}
                component="pre"
                sx={{ m: 0, p: 1.5, borderRadius: 1, border: "1px solid", borderColor: "divider", bgcolor: "surface.containerLow", fontFamily: mono, fontSize: "12.5px", overflowX: "auto" }}
              >
                {block.text}
              </Box>
            );
          default:
            return <Box key={index} component="p" sx={{ m: 0, whiteSpace: "pre-line" }}>{inlineMarkdown(block.text)}</Box>;
        }
      })}
    </Box>
  );
}

function Verification({ preview }: { preview: ActionPreview }) {
  if (preview.verify_status !== "passed" && preview.verify_status !== "failed") {
    return <Typography sx={{ ...overviewTypography.description, color: "text.secondary" }}>No automated verification ran.</Typography>;
  }
  const passed = preview.verify_status === "passed";
  const accent = passed ? "success" : "error";
  const Icon = passed ? CheckCircleOutlined : ErrorOutlined;
  return (
    <Box sx={{ display: "grid", gridTemplateColumns: "auto minmax(0, 1fr)", columnGap: 1, alignItems: "start" }}>
      <Icon sx={(theme) => ({ fontSize: 18, mt: "1px", ...accentLabelSx(theme, accent) })} />
      <Box sx={{ minWidth: 0 }}>
        <Typography sx={(theme) => ({ ...overviewTypography.secondaryBody, fontWeight: 650, ...accentLabelSx(theme, accent) })}>
          {passed ? "Verification passed" : "Verification failed"}
        </Typography>
        {preview.verify_summary && (
          <Typography sx={{ ...overviewTypography.description, color: "text.secondary", mt: 0.25, overflowWrap: "anywhere" }}>
            {preview.verify_summary}
          </Typography>
        )}
        {!passed && (
          <Typography sx={{ ...overviewTypography.description, color: "text.secondary", mt: 0.25 }}>
            The change likely does not build as-is; treat it as a lead.
          </Typography>
        )}
      </Box>
    </Box>
  );
}

// verificationState summarizes the validator result for the header button.
function verificationState(preview: ActionPreview) {
  if (preview.verify_status === "passed") return { accent: "success" as const, Icon: CheckCircleOutlined, label: "Checks passed" };
  if (preview.verify_status === "failed") return { accent: "error" as const, Icon: ErrorOutlined, label: "Checks failed" };
  return { accent: null, Icon: CheckCircleOutlined, label: "No checks" };
}

// The header names the check result; the summary and any failure note open on
// demand so they do not crowd the title.
function VerificationChecksButton({ preview }: { preview: ActionPreview }) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const state = verificationState(preview);
  const tone = (theme: Parameters<typeof accentLabelSx>[0]) => (state.accent ? accentLabelSx(theme, state.accent) : { color: "text.secondary" });
  return (
    <Box sx={{ display: { xs: "none", md: "inline-flex" }, flexShrink: 0 }}>
      <Button
        size="small"
        variant="outlined"
        color="inherit"
        aria-haspopup="dialog"
        aria-expanded={Boolean(anchor)}
        onClick={(event) => setAnchor(event.currentTarget)}
        startIcon={<state.Icon sx={(theme) => ({ fontSize: 18, ...tone(theme) })} />}
        endIcon={<ExpandMoreOutlined sx={{ color: "text.secondary" }} />}
        sx={{ borderColor: "divider", fontWeight: 650, "&:hover": { borderColor: "text.secondary" } }}
      >
        <Box component="span" sx={tone}>{state.label}</Box>
      </Button>
      <Popover
        open={Boolean(anchor)}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        slotProps={{ paper: { sx: { ...overlayPaperSx, mt: 0.75, p: 2, maxWidth: 380 } } }}
      >
        <Typography component="h3" sx={dialogRegionLabelSx}>Automated verification</Typography>
        <Verification preview={preview} />
      </Popover>
    </Box>
  );
}

function Kbd({ children }: { children: ReactNode }) {
  return (
    <Box
      component="kbd"
      sx={{ fontFamily: mono, fontSize: "11px", lineHeight: "16px", px: 0.6, borderRadius: 0.5, border: "1px solid", borderColor: "divider", bgcolor: "surface.container" }}
    >
      {children}
    </Box>
  );
}

function prefersReducedMotion(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

/**
 * The fix draft's review step: the diff leads, the description sits above it,
 * and revision and confirmation stay in one action bar. J and K move between
 * hunks, / focuses the revision, and Cmd/Ctrl+Enter submits the revision from
 * that field or opens the draft PR from anywhere else.
 */
export function FixReviewWorkspace(props: FixReviewProps) {
  const { preview } = props;
  const files = useMemo(() => parseUnifiedDiff(preview.diff ?? ""), [preview.diff]);
  const totals = diffTotals(files);
  // A file list earns its width only when there is more than one file to pick.
  const multiFile = files.length > 1;
  const [descriptionOpen, setDescriptionOpen] = useState(
    () => stripMarkdownComments(preview.body).length <= shortDescriptionLength,
  );
  const rootRef = useRef<HTMLDivElement>(null);
  const mainRef = useRef<HTMLDivElement>(null);
  const revisionRef = useRef<HTMLInputElement>(null);
  const hunkIndex = useRef(-1);

  // Keyboard shortcuts listen on this element, so take focus unless the
  // operator is already inside it.
  useEffect(() => {
    if (!rootRef.current?.contains(document.activeElement)) rootRef.current?.focus({ preventScroll: true });
  }, []);

  const jump = (step: number) => {
    const hunks = [...(mainRef.current?.querySelectorAll<HTMLElement>("[data-diff-hunk]") ?? [])];
    if (hunks.length === 0) return;
    hunkIndex.current = Math.min(hunks.length - 1, Math.max(0, hunkIndex.current + step));
    const target = hunks[hunkIndex.current];
    target.scrollIntoView({ block: "start", behavior: prefersReducedMotion() ? "auto" : "smooth" });
    target.focus({ preventScroll: true });
  };
  const scrollToFile = (path: string) => {
    const file = mainRef.current?.querySelector<HTMLElement>(`[data-diff-file="${CSS.escape(path)}"]`);
    if (!file) return;
    // J and K continue from the file the operator picked.
    const hunks = [...(mainRef.current?.querySelectorAll<HTMLElement>("[data-diff-hunk]") ?? [])];
    const first = file.querySelector<HTMLElement>("[data-diff-hunk]");
    if (first) hunkIndex.current = hunks.indexOf(first);
    file.scrollIntoView({ block: "start", behavior: prefersReducedMotion() ? "auto" : "smooth" });
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    // Enter that finishes an IME composition is not a shortcut.
    if (event.nativeEvent.isComposing) return;
    const inField = Boolean((event.target as HTMLElement).closest("input, textarea"));
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      if (inField) {
        if (!props.reviseDisabled) props.onRevise();
      } else if (props.busy === null) {
        props.onConfirm();
      }
      return;
    }
    if (inField || event.metaKey || event.ctrlKey || event.altKey) return;
    if (event.key === "j") {
      event.preventDefault();
      jump(1);
    } else if (event.key === "k") {
      event.preventDefault();
      jump(-1);
    } else if (event.key === "/" && props.canRevise) {
      event.preventDefault();
      revisionRef.current?.focus();
    }
  };

  return (
    <Box
      ref={rootRef}
      tabIndex={-1}
      onKeyDown={onKeyDown}
      sx={{ flex: 1, minHeight: 0, display: "grid", gridTemplateRows: "auto minmax(0, 1fr) auto", outline: "none" }}
    >
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 1.5,
          px: { xs: 1, md: 2 },
          py: 1,
          minHeight: 56,
          borderBottom: "1px solid",
          ...sectionBandSx(),
        }}
      >
        <IconButton aria-label="Back to context" onClick={props.onBack} disabled={props.busy !== null} size="small">
          <ArrowBackOutlined fontSize="small" />
        </IconButton>
        <Box sx={{ minWidth: 0, flex: 1 }}>
          <DialogTitle
            title={preview.title}
            sx={{
              p: 0,
              ...overviewTypography.majorHeading,
              display: "-webkit-box",
              WebkitLineClamp: 2,
              WebkitBoxOrient: "vertical",
              overflow: "hidden",
              overflowWrap: "anywhere",
            }}
          >
            {preview.title}
          </DialogTitle>
          <Box sx={{ display: "flex", flexWrap: "wrap", columnGap: 1.25, alignItems: "center", ...overviewTypography.description, color: "text.secondary" }}>
            <span>
              Draft pull request
              {props.branch && <> into <Box component="code" sx={{ fontFamily: mono, color: "text.primary" }}>{props.branch}</Box></>}
            </span>
            {files.length > 0 && <ChangeCounts {...totals} />}
          </Box>
        </Box>
        <VerificationChecksButton preview={preview} />
        <IconButton aria-label="Close" onClick={props.onCancel} disabled={props.busy === "confirm"}>
          <CloseOutlined fontSize="small" />
        </IconButton>
      </Box>

      <Box sx={{ display: "grid", gridTemplateColumns: { xs: "minmax(0, 1fr)", md: multiFile ? "248px minmax(0, 1fr)" : "minmax(0, 1fr)" }, minHeight: 0 }}>
        {multiFile && (
          <Box
            component="nav"
            aria-label="Changed files"
            sx={{ display: { xs: "none", md: "block" }, overflowY: "auto", borderRight: "1px solid", borderColor: "divider", bgcolor: "surface.containerLow", py: 1.5 }}
          >
            <Typography sx={{ ...dialogRegionLabelSx, px: 2 }}>{files.length} files changed</Typography>
            {files.map((file) => {
              const slash = file.path.lastIndexOf("/");
              return (
                <ButtonBase
                  key={file.path}
                  onClick={() => scrollToFile(file.path)}
                  sx={{ display: "grid", width: "100%", justifyItems: "start", textAlign: "left", px: 2, py: 0.75, "&:hover": { bgcolor: "surface.containerHigh" }, ...focusRingSx }}
                >
                  <Typography sx={{ ...overviewTypography.data, fontWeight: 650, overflowWrap: "anywhere" }}>{file.path.slice(slash + 1)}</Typography>
                  <Box sx={{ display: "flex", gap: 1, width: "100%", alignItems: "baseline" }}>
                    <Typography sx={{ ...overviewTypography.description, color: "text.secondary", flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                      {slash > 0 ? file.path.slice(0, slash) : "repository root"}
                    </Typography>
                    <ChangeCounts additions={file.additions} deletions={file.deletions} />
                  </Box>
                </ButtonBase>
              );
            })}
          </Box>
        )}

        <Box ref={mainRef} sx={{ overflowY: "auto", p: { xs: 1.5, md: 2.5 }, display: "grid", alignContent: "start", gap: 2 }}>
          <Box sx={{ display: { xs: "block", md: "none" } }}>
            <Verification preview={preview} />
          </Box>
          {props.error && <Alert severity="error" variant="outlined">{props.error}</Alert>}
          {props.observation && <Alert role="status" severity="info" variant="outlined">{props.observation}</Alert>}
          {preview.warnings?.length ? (
            <Alert severity="warning" variant="outlined">
              <Typography variant="body2" sx={{ fontWeight: 700 }}>Investigation warning</Typography>
              {preview.warnings.map((warning) => <Typography key={warning.message} variant="body2">{warning.message}</Typography>)}
            </Alert>
          ) : null}
          {preview.verify_status === "failed" && preview.verify_output && (
            <Box component="section">
              <Typography component="h3" sx={dialogRegionLabelSx}>Verification output</Typography>
              <Box component="pre" sx={{ m: 0, p: 1.5, maxHeight: 200, overflow: "auto", borderRadius: 1, border: "1px solid", borderColor: "divider", bgcolor: "surface.containerLow", fontFamily: mono, fontSize: "12.5px" }}>
                {preview.verify_output}
              </Box>
            </Box>
          )}
          <Box sx={{ border: "1px solid", borderColor: "divider", borderRadius: 1 }}>
            <ButtonBase
              onClick={() => setDescriptionOpen((open) => !open)}
              aria-expanded={descriptionOpen}
              sx={{ display: "flex", width: "100%", justifyContent: "space-between", px: 1.5, minHeight: 40, bgcolor: "surface.containerHigh", ...focusRingSx }}
            >
              <Typography component="span" sx={overviewTypography.subsectionHeading}>Description</Typography>
              <ExpandMoreOutlined
                sx={{
                  fontSize: 20,
                  color: "text.secondary",
                  transform: descriptionOpen ? "rotate(180deg)" : "none",
                  transition: `transform 200ms ${easeOut}`,
                  "@media (prefers-reduced-motion: reduce)": { transition: "none" },
                }}
              />
            </ButtonBase>
            {/* Collapsed, the opening lines stay readable and fade toward the toggle. */}
            <Box
              sx={{
                px: 2,
                py: 1.5,
                maxHeight: descriptionOpen ? "none" : 112,
                overflow: "hidden",
                maskImage: descriptionOpen ? "none" : "linear-gradient(180deg, #000 55%, transparent)",
              }}
            >
              <MarkdownBody text={preview.body} />
            </Box>
          </Box>
          {files.length > 0 && <DiffView files={files} />}
        </Box>
      </Box>

      <Box
        sx={{
          display: "flex",
          flexWrap: "wrap",
          alignItems: "center",
          gap: 1.5,
          px: 2,
          py: 1.25,
          borderTop: "1px solid",
          borderColor: "divider",
          bgcolor: "surface.containerHigh",
        }}
      >
        {props.generating ? (
          <Box role="status" sx={{ flex: "1 1 20rem", display: "flex", gap: 1.25, alignItems: "center", minWidth: 0 }}>
            <CircularProgress size={18} sx={{ flexShrink: 0 }} />
            <Box sx={{ minWidth: 0 }}>
              <Typography variant="body2" sx={{ fontWeight: 700 }}>Regenerating the fix preview</Typography>
              {props.sentInstruction && (
                <Typography variant="caption" color="textSecondary" sx={{ display: "block", overflowWrap: "anywhere" }}>
                  {props.sentInstruction}
                </Typography>
              )}
            </Box>
          </Box>
        ) : props.canRevise ? (
          <Box sx={{ flex: "1 1 20rem", display: "flex", gap: 1, alignItems: "flex-start" }}>
            {/* Multiline so a carried-over instruction keeps its line breaks;
                Cmd/Ctrl+Enter regenerates. */}
            <TextField
              size="small"
              fullWidth
              multiline
              maxRows={3}
              placeholder="Describe the revision needed for the next preview"
              value={props.instruction}
              onChange={(event) => props.onInstruction(event.target.value)}
              helperText={`${props.instructionBytes}/4096 bytes`}
              inputRef={revisionRef}
              slotProps={{ htmlInput: { "aria-label": "Revision instruction" } }}
            />
            <Button variant="outlined" onClick={props.onRevise} disabled={props.reviseDisabled} sx={{ flexShrink: 0 }}>
              Regenerate
            </Button>
          </Box>
        ) : (
          <Box sx={{ flex: 1 }} />
        )}
        <Box sx={{ display: { xs: "none", lg: "flex" }, gap: 0.75, alignItems: "center", ...overviewTypography.description, color: "text.secondary" }}>
          <Kbd>J</Kbd><Kbd>K</Kbd> hunks
          {props.canRevise && <><Box component="span" sx={{ mx: 0.5 }}>·</Box><Kbd>/</Kbd> revise</>}
          <Box component="span" sx={{ mx: 0.5 }}>·</Box>
          <Kbd>⌘</Kbd><Kbd>↵</Kbd> open
        </Box>
        <Button color="inherit" onClick={props.onCancel} disabled={props.busy === "confirm"}>Cancel</Button>
        <Button
          variant="contained"
          startIcon={props.busy === "confirm" ? <CircularProgress size={16} color="inherit" /> : <BuildOutlined />}
          onClick={props.onConfirm}
          disabled={props.busy !== null}
        >
          {props.busy === "confirm" ? "Opening draft PR" : props.hasWarnings ? "Open draft PR with warnings" : "Open draft PR"}
        </Button>
      </Box>
    </Box>
  );
}
