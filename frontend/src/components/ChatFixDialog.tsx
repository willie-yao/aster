import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import FormControl from "@mui/material/FormControl";
import InputLabel from "@mui/material/InputLabel";
import Link from "@mui/material/Link";
import MenuItem from "@mui/material/MenuItem";
import Select from "@mui/material/Select";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import {
  ArrowBackOutlined,
  BuildOutlined,
  CheckCircleOutlined,
  FactCheckOutlined,
  SourceOutlined,
} from "@mui/icons-material";
import {
  chatFixInstructionBytes,
  confirmChatFix,
  limitChatFixInstruction,
  previewChatFix,
  type ChatFixPreview,
} from "../lib/chatFix";
import { soft } from "../theme";
import type { AnalysisChatMessage } from "../types/analysisChat";
import type { PatternAnalysis } from "../types/dashboard";
import type { SourceInvestigationView } from "../types/sourceInvestigation";
import { ActionDraftPreview } from "./ActionDraftPreview";
import { RichText } from "./RichText";

export interface ChatFixSourceSelection {
  requestID: string;
  view: SourceInvestigationView;
}

function EvidenceList({
  citations,
}: {
  citations: { path: string; line_start?: number; line_end?: number; quote?: string }[];
}) {
  return (
    <Stack spacing={0.8}>
      {citations.map((citation, index) => {
        const lines = citation.line_start
          ? citation.line_end && citation.line_end !== citation.line_start
            ? `lines ${citation.line_start}-${citation.line_end}`
            : `line ${citation.line_start}`
          : "";
        return (
          <Box
            key={`${citation.path}-${citation.line_start ?? 0}-${index}`}
            sx={{ borderLeft: "2px solid", borderColor: "success.main", pl: 1.1, py: 0.15 }}
          >
            <Stack direction="row" spacing={0.7} useFlexGap sx={{ alignItems: "baseline", flexWrap: "wrap" }}>
              <Typography sx={{ fontFamily: "monospace", fontSize: "0.75rem", fontWeight: 700 }}>
                {citation.path}
              </Typography>
              {lines && (
                <Typography variant="caption" color="text.secondary">
                  {lines}
                </Typography>
              )}
            </Stack>
            {citation.quote && (
              <Typography
                component="blockquote"
                variant="caption"
                color="text.secondary"
                sx={{ m: 0, mt: 0.3, fontFamily: "monospace", lineHeight: 1.5 }}
              >
                “{citation.quote}”
              </Typography>
            )}
          </Box>
        );
      })}
    </Stack>
  );
}

function ContextSection({ title, icon, children }: {
  title: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <Box>
      <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", mb: 0.9 }}>
        {icon}
        <Typography variant="label" sx={{ fontWeight: 750 }}>
          {title}
        </Typography>
      </Stack>
      {children}
    </Box>
  );
}

export function ChatFixDialog({
  open,
  sessionID,
  message,
  patterns,
  source,
  onClose,
}: {
  open: boolean;
  sessionID: string;
  message: AnalysisChatMessage | null;
  patterns: PatternAnalysis[];
  source: ChatFixSourceSelection | null;
  onClose: () => void;
}) {
  const [patternID, setPatternID] = useState("");
  const [instruction, setInstruction] = useState("");
  const [preview, setPreview] = useState<ChatFixPreview | null>(null);
  const [busy, setBusy] = useState<"preview" | "confirm" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [url, setURL] = useState<string | null>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const identity = `${sessionID}\u0000${message?.request_id ?? ""}`;
  const identityRef = useRef(identity);
  identityRef.current = identity;

  const eligiblePatterns = useMemo(
    () => patterns.filter(
      (pattern): pattern is PatternAnalysis & { id: string; content_hash: string } =>
        Boolean(pattern.id && pattern.content_hash),
    ),
    [patterns],
  );
  const selectedPattern = eligiblePatterns.find((pattern) => pattern.id === patternID) ?? null;
  const sourceResult = source?.view.status === "succeeded" ? source.view.result : undefined;

  const firstPatternID = eligiblePatterns[0]?.id ?? "";

  useEffect(() => {
    if (!open) return;
    controllerRef.current?.abort();
    setPatternID(firstPatternID);
    setInstruction("");
    setPreview(null);
    setBusy(null);
    setError(null);
    setURL(null);
  }, [identity, firstPatternID, open]);

  useEffect(() => () => controllerRef.current?.abort(), []);

  async function generatePreview() {
    if (!message?.request_id || !selectedPattern || busy) return;
    const requestIdentity = identity;
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy("preview");
    setError(null);
    try {
      const value = await previewChatFix(
        sessionID,
        message.request_id,
        patternID,
        selectedPattern.content_hash,
        sourceResult && source ? source.requestID : null,
        instruction,
        controller.signal,
      );
      if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
      setPreview(value);
    } catch (previewError) {
      if (previewError instanceof Error && previewError.name === "AbortError") return;
      if (identityRef.current === requestIdentity) {
        setError(previewError instanceof Error ? previewError.message : "Could not generate the fix preview.");
      }
    } finally {
      if (controllerRef.current === controller) {
        controllerRef.current = null;
        if (identityRef.current === requestIdentity) setBusy(null);
      }
    }
  }

  async function confirm() {
    if (!preview?.token || busy) return;
    const requestIdentity = identity;
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy("confirm");
    setError(null);
    try {
      const resultURL = await confirmChatFix(preview.token, controller.signal);
      if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
      setURL(resultURL);
    } catch (confirmError) {
      if (confirmError instanceof Error && confirmError.name === "AbortError") return;
      if (identityRef.current === requestIdentity) {
        setError(confirmError instanceof Error ? confirmError.message : "Could not open the draft PR.");
      }
    } finally {
      if (controllerRef.current === controller) {
        controllerRef.current = null;
        if (identityRef.current === requestIdentity) setBusy(null);
      }
    }
  }

  function close() {
    if (busy) return;
    controllerRef.current?.abort();
    onClose();
  }

  if (!message) return null;

  return (
    <Dialog
      open={open}
      onClose={busy ? undefined : close}
      maxWidth="md"
      fullWidth
      slotProps={{
        paper: {
          sx: {
            borderRadius: "16px",
            border: "1px solid",
            borderColor: "divider",
            backgroundImage: "none",
          },
        },
      }}
    >
      <DialogTitle sx={{ display: "flex", alignItems: "center", gap: 1.25, px: 3, py: 2.25 }}>
        <Box
          sx={{
            width: 38,
            height: 38,
            display: "grid",
            placeItems: "center",
            borderRadius: "11px",
            color: "warning.main",
            bgcolor: (theme) => soft(theme, "warning", 0.13),
            border: "1px solid",
            borderColor: (theme) => soft(theme, "warning", 0.28),
          }}
        >
          <BuildOutlined sx={{ fontSize: 20 }} />
        </Box>
        <Box>
          <Typography variant="headline" component="span" sx={{ display: "block", fontSize: "1.125rem" }}>
            Use this finding in a fix proposal
          </Typography>
          <Typography variant="caption" color="text.secondary">
            Review the exact context before the coding agent sees it.
          </Typography>
        </Box>
      </DialogTitle>

      <DialogContent dividers sx={{ px: 3, py: 2.5 }}>
        {error && <Alert severity="error" variant="outlined" sx={{ mb: 2 }}>{error}</Alert>}
        {url && (
          <Alert severity="success" icon={<CheckCircleOutlined />} sx={{ mb: 2 }}>
            Draft PR opened: <Link href={url} target="_blank" rel="noopener noreferrer">{url}</Link>
          </Alert>
        )}

        {!preview && !url && (
          <Stack spacing={2.5}>
            <Alert severity="info" variant="outlined">
              Only this response, its verified evidence, the selected recurring pattern, any enabled verified source finding, and your optional instruction are sent. The complete conversation is excluded.
            </Alert>

            <ContextSection title="Recurring pattern" icon={<BuildOutlined sx={{ fontSize: 17, color: "warning.main" }} />}>
              {eligiblePatterns.length > 1 && (
                <FormControl fullWidth size="small" sx={{ mb: 1.25 }}>
                  <InputLabel id="chat-fix-pattern-label">Pattern</InputLabel>
                  <Select
                    labelId="chat-fix-pattern-label"
                    label="Pattern"
                    value={patternID}
                    onChange={(event) => setPatternID(event.target.value)}
                  >
                    {eligiblePatterns.map((pattern) => (
                      <MenuItem key={pattern.id} value={pattern.id}>{pattern.subject}</MenuItem>
                    ))}
                  </Select>
                </FormControl>
              )}
              {selectedPattern && (
                <Box sx={{ borderRadius: "10px", bgcolor: "action.selected", px: 1.5, py: 1.25 }}>
                  <Typography variant="body2" sx={{ fontWeight: 700 }}>{selectedPattern.subject}</Typography>
                  {selectedPattern.shared_root_cause && (
                    <Typography variant="body2" color="text.secondary" sx={{ mt: 0.55, lineHeight: 1.55 }}>
                      <RichText text={selectedPattern.shared_root_cause} steps />
                    </Typography>
                  )}
                  {selectedPattern.suggested_fix && (
                    <Typography variant="caption" color="primary.main" sx={{ display: "block", mt: 0.8, fontWeight: 700 }}>
                      Direction: {selectedPattern.suggested_fix}
                    </Typography>
                  )}
                  {selectedPattern.relevant_files && selectedPattern.relevant_files.length > 0 && (
                    <Box sx={{ mt: 1 }}>
                      <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 700, mb: 0.45 }}>
                        Agent starting files
                      </Typography>
                      <Box
                        sx={{
                          borderRadius: "8px",
                          bgcolor: "background.paper",
                          border: "1px solid",
                          borderColor: "divider",
                          px: 1,
                          py: 0.65,
                        }}
                      >
                        {selectedPattern.relevant_files.map((path) => (
                          <Typography
                            key={path}
                            variant="caption"
                            sx={{ display: "block", fontFamily: "monospace", overflowWrap: "anywhere" }}
                          >
                            {path}
                          </Typography>
                        ))}
                      </Box>
                    </Box>
                  )}
                </Box>
              )}
            </ContextSection>

            <ContextSection title="Selected chat finding" icon={<FactCheckOutlined sx={{ fontSize: 17, color: "success.main" }} />}>
              <Box sx={{ borderLeft: "3px solid", borderColor: "primary.main", pl: 1.5, py: 0.2 }}>
                <Typography variant="body2" sx={{ lineHeight: 1.6 }}>
                  <RichText text={message.content} steps />
                </Typography>
              </Box>
              {message.proposed_revision && (
                <Box sx={{ mt: 1.2, borderRadius: "10px", bgcolor: (theme) => soft(theme, "warning", 0.07), p: 1.25 }}>
                  <Typography variant="caption" color="warning.main" sx={{ fontWeight: 750 }}>Evidence-backed revision</Typography>
                  <Typography variant="body2" sx={{ mt: 0.45 }}>{message.proposed_revision.root_cause}</Typography>
                  <Typography variant="body2" color="text.secondary" sx={{ mt: 0.45 }}>{message.proposed_revision.suggested_fix}</Typography>
                </Box>
              )}
              {message.citations && message.citations.length > 0 && (
                <Box sx={{ mt: 1.2 }}>
                  <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 700, mb: 0.65 }}>
                    Verified artifact evidence
                  </Typography>
                  <EvidenceList citations={message.citations} />
                </Box>
              )}
            </ContextSection>

            {sourceResult && source ? (
              <ContextSection title="Required verified source investigation" icon={<SourceOutlined sx={{ fontSize: 17, color: "info.main" }} />}>
                <Box sx={{ borderRadius: "10px", bgcolor: (theme) => soft(theme, "info", 0.06), p: 1.25 }}>
                  <Typography variant="body2" sx={{ lineHeight: 1.6 }}>{sourceResult.finding}</Typography>
                  {sourceResult.target?.path && (
                    <Typography variant="caption" color="text.secondary">Target: {sourceResult.target.path}</Typography>
                  )}
                  {sourceResult.citations && sourceResult.citations.length > 0 && (
                    <Box sx={{ mt: 1 }}><EvidenceList citations={sourceResult.citations} /></Box>
                  )}
                </Box>
              </ContextSection>
            ) : (
              <Alert severity="warning">A completed actionable source investigation is required before fix generation.</Alert>
            )}

            <TextField
              label="Maintainer instruction (optional)"
              placeholder="e.g. preserve backward compatibility and change only the controller retry branch"
              fullWidth
              multiline
              minRows={2}
              maxRows={5}
              value={instruction}
              onChange={(event) => setInstruction(limitChatFixInstruction(event.target.value))}
              helperText={`${chatFixInstructionBytes(instruction)}/4096 bytes`}
            />
          </Stack>
        )}

        {busy === "preview" && (
          <Stack direction="row" spacing={1.25} sx={{ alignItems: "center", py: 4, justifyContent: "center" }}>
            <CircularProgress size={20} />
            <Box>
              <Typography variant="body2" sx={{ fontWeight: 700 }}>Generating the fix preview</Typography>
              <Typography variant="caption" color="text.secondary">The coding agent is using only the reviewed context.</Typography>
            </Box>
          </Stack>
        )}

        {preview && !url && (
          <Stack spacing={2.25}>
            <Button
              size="small"
              color="inherit"
              startIcon={<ArrowBackOutlined />}
              onClick={() => setPreview(null)}
              disabled={busy !== null}
              sx={{ alignSelf: "flex-start" }}
            >
              Back to context
            </Button>
            <ActionDraftPreview preview={preview} />
          </Stack>
        )}
      </DialogContent>

      <DialogActions sx={{ px: 3, py: 2 }}>
        <Button onClick={close} disabled={busy !== null}>{url ? "Done" : "Cancel"}</Button>
        {!preview && !url && (
          <Button
            variant="contained"
            color="warning"
            startIcon={busy === "preview" ? <CircularProgress size={16} color="inherit" /> : <BuildOutlined />}
            onClick={() => void generatePreview()}
            disabled={busy !== null || !patternID || !sourceResult || !source}
          >
            {busy === "preview" ? "Generating" : "Generate fix preview"}
          </Button>
        )}
        {preview && !url && (
          <Button
            variant="contained"
            color="warning"
            startIcon={busy === "confirm" ? <CircularProgress size={16} color="inherit" /> : <BuildOutlined />}
            onClick={() => void confirm()}
            disabled={busy !== null}
          >
            {busy === "confirm" ? "Opening draft PR" : "Open draft PR"}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
