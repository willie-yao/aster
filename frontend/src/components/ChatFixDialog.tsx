import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import CircularProgress from "@mui/material/CircularProgress";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
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
} from "@mui/icons-material";
import {
  cancelAnalysisChatFixRequest,
  chatFixInstructionBytes,
  confirmChatFix,
  createAnalysisChatFixRequest,
  limitChatFixInstruction,
  loadAnalysisChatFixRequest,
  previewChatFix,
  type ChatFixRequest,
  type ChatFixPreview,
} from "../lib/chatFix";
import { chatFixRequestPresentation } from "../lib/chatFixPresentation";
import { actionRequestIsPollable } from "../lib/actionRequests";
import { clearStoredChatFixRequest, readStoredChatFixRequest, storeChatFixRequest } from "../lib/chatFixRequestStorage";
import type { AnalysisChatMessage } from "../types/analysisChat";
import type { PatternAnalysis } from "../types/dashboard";
import { ActionDraftPreview } from "./ActionDraftPreview";
import { DialogHeader } from "./ActionDialog";
import { dialogGutter, dialogPaperSx } from "../theme/overview";
import { RichText } from "./RichText";
import { alertRole } from "../theme";

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
                <Typography variant="caption" color="textSecondary">
                  {lines}
                </Typography>
              )}
            </Stack>
            {citation.quote && (
              <Typography
                component="blockquote"
                variant="caption"
                color="textSecondary"
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
  exactAnalysis,
  causeScope = false,
  onClose,
}: {
  open: boolean;
  sessionID: string;
  message: AnalysisChatMessage | null;
  patterns: PatternAnalysis[];
  exactAnalysis: boolean;
  causeScope?: boolean;
  onClose: () => void;
}) {
  const [patternID, setPatternID] = useState("");
  const [instruction, setInstruction] = useState("");
  const [submittedInstruction, setSubmittedInstruction] = useState("");
  const [preview, setPreview] = useState<ChatFixPreview | null>(null);
  const [request, setRequest] = useState<ChatFixRequest | null>(null);
  const [busy, setBusy] = useState<"preview" | "regenerate" | "confirm" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [observationMessage, setObservationMessage] = useState<string | null>(null);
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
  const requestPresentation = exactAnalysis && request ? chatFixRequestPresentation(request) : null;
  const isProviderCredentialRetry = request?.status === "failed"
    && request.failure?.category === "provider_credential";
  const hasRevisedInstruction = Boolean(
    instruction.trim() && instruction.trim() !== submittedInstruction.trim(),
  );
  const instructionHelperText = requestPresentation?.canRegenerate && !isProviderCredentialRetry && !hasRevisedInstruction
    ? `Change the previous instruction to enable regeneration. ${chatFixInstructionBytes(instruction)}/4096 bytes`
    : `${chatFixInstructionBytes(instruction)}/4096 bytes`;

  const firstPatternID = eligiblePatterns[0]?.id ?? "";

  useEffect(() => {
    if (!open) return;
    controllerRef.current?.abort();
    setPatternID(firstPatternID);
    setInstruction("");
    setSubmittedInstruction("");
    setPreview(null);
    setRequest(null);
    setBusy(null);
    setError(null);
    setObservationMessage(null);
    setURL(null);
  }, [identity, firstPatternID, open]);

  useEffect(() => () => controllerRef.current?.abort(), []);

  const observeAnalysisFixRequest = useCallback(async (
    id: string,
    requestIdentity: string,
    controller: AbortController,
    initial?: ChatFixRequest,
  ): Promise<void> => {
    let current = initial ?? await loadAnalysisChatFixRequest(id, controller.signal);
    for (;;) {
      if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
      setRequest(current);
      if (current.status === "ready") {
        if (!current.preview?.token) throw new Error("The generated fix preview is incomplete.");
        setPreview(current.preview);
        return;
      }
      if (!actionRequestIsPollable(current.status) && current.status !== "unknown") {
        return;
      }
      await new Promise((resolve) => window.setTimeout(resolve, 1000));
      if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
      current = await loadAnalysisChatFixRequest(current.id, controller.signal);
    }
  }, []);

  useEffect(() => {
    if (!open || !exactAnalysis || !message?.request_id) return;
    const chatRequestID = message.request_id;
    const stored = readStoredChatFixRequest(window.sessionStorage, sessionID, chatRequestID);
    if (!stored) return;
    const requestIdentity = identity;
    const controller = new AbortController();
    controllerRef.current?.abort();
    controllerRef.current = controller;
    setInstruction(stored.instruction);
    setSubmittedInstruction(stored.instruction);
    setBusy("preview");
    setError(null);
    setObservationMessage(null);
    void observeAnalysisFixRequest(stored.id, requestIdentity, controller)
      .catch((requestError) => {
        if (requestError instanceof Error && requestError.name === "AbortError") return;
        if (identityRef.current === requestIdentity) {
          setObservationMessage("The admitted request could not be observed just now. Reopen this dialog to continue observing the same request ID.");
        }
      })
      .finally(() => {
        if (controllerRef.current === controller) {
          controllerRef.current = null;
          if (identityRef.current === requestIdentity) setBusy(null);
        }
      });
    return () => {
      if (controllerRef.current === controller) {
        controller.abort();
        controllerRef.current = null;
      }
    };
  }, [exactAnalysis, identity, message?.request_id, observeAnalysisFixRequest, open, sessionID]);


  async function generatePreview() {
    if (!message?.request_id || busy || (!exactAnalysis && !selectedPattern)) return;
    const requestIdentity = identity;
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy("preview");
    setError(null);
    setObservationMessage(null);
    let admitted = false;
    try {
      if (exactAnalysis) {
        const request = await createAnalysisChatFixRequest(
          sessionID,
          message.request_id,
          instruction,
          undefined,
          controller.signal,
        );
        if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
        storeChatFixRequest(window.sessionStorage, sessionID, message.request_id, { id: request.id, instruction });
        setSubmittedInstruction(instruction);
        setRequest(request);
        admitted = true;
        await observeAnalysisFixRequest(request.id, requestIdentity, controller, request);
        return;
      }
      const value = await previewChatFix(
        sessionID,
        message.request_id,
        patternID,
        selectedPattern?.content_hash ?? null,
        instruction,
        controller.signal,
      );
      if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
      setPreview(value);
    } catch (previewError) {
      if (previewError instanceof Error && previewError.name === "AbortError") return;
      if (identityRef.current === requestIdentity) {
        const detail = previewError instanceof Error ? previewError.message : "Could not generate the fix preview.";
        if (exactAnalysis && admitted) {
          setObservationMessage("The admitted request could not be observed just now. Reopen this dialog to continue observing the same request ID.");
        } else {
          setError(detail);
        }
      }
    } finally {
      if (controllerRef.current === controller) {
        controllerRef.current = null;
        if (identityRef.current === requestIdentity) setBusy(null);
      }
    }
  }

  async function regeneratePreview() {
    if (!exactAnalysis || !message?.request_id || !request?.id || busy) return;
    const providerCredentialRetry = request.status === "failed" && request.failure?.category === "provider_credential";
    if (!providerCredentialRetry && (!instruction.trim() || instruction.trim() === submittedInstruction.trim())) {
      setError("Edit the maintainer instruction before regenerating the preview.");
      return;
    }
    const requestIdentity = identity;
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy("regenerate");
    setError(null);
    setObservationMessage(null);
    try {
      const feedbackReplacement = request.status === "failed" && request.failure?.category === "no_reviewable_patch";
      if (!providerCredentialRetry && !feedbackReplacement) {
        let cancelled = await cancelAnalysisChatFixRequest(request.id);
        while (actionRequestIsPollable(cancelled.status)) {
          await new Promise((resolve) => window.setTimeout(resolve, 1000));
          if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
          cancelled = await loadAnalysisChatFixRequest(request.id, controller.signal);
        }
        if (cancelled.status !== "cancelled") {
          throw new Error(cancelled.error || "The previous fix preview could not be cancelled.");
        }
        if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
        setRequest(cancelled);
        setPreview(null);
        clearStoredChatFixRequest(window.sessionStorage, sessionID, message.request_id);
      }

      const replacement = await createAnalysisChatFixRequest(
        sessionID,
        message.request_id,
        instruction,
        feedbackReplacement ? request.id : undefined,
        controller.signal,
      );
      if (identityRef.current !== requestIdentity || controllerRef.current !== controller) return;
      if (providerCredentialRetry) {
        clearStoredChatFixRequest(window.sessionStorage, sessionID, message.request_id);
        setPreview(null);
      }
      storeChatFixRequest(window.sessionStorage, sessionID, message.request_id, { id: replacement.id, instruction });
      setSubmittedInstruction(instruction);
      setRequest(replacement);
      await observeAnalysisFixRequest(replacement.id, requestIdentity, controller, replacement);
    } catch (regenerateError) {
      if (regenerateError instanceof Error && regenerateError.name === "AbortError") return;
      if (identityRef.current === requestIdentity) {
        setError(regenerateError instanceof Error ? regenerateError.message : "Could not regenerate the fix preview.");
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
      if (message?.request_id) clearStoredChatFixRequest(window.sessionStorage, sessionID, message.request_id);
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
    if (busy && !(exactAnalysis && busy === "preview")) return;
    controllerRef.current?.abort();
    onClose();
  }

  if (!message) return null;

  return (
    <Dialog
      open={open}
      onClose={busy && !(exactAnalysis && busy === "preview") ? undefined : close}
      maxWidth="md"
      fullWidth
      slotProps={{ paper: { sx: dialogPaperSx } }}
    >
      <DialogHeader
        icon={<BuildOutlined sx={{ fontSize: 18 }} />}
        title="Use this finding in a fix proposal"
        subtitle="Review the exact context before the coding agent sees it."
      />

      <DialogContent dividers sx={{ px: dialogGutter, py: 2 }}>
        {error && <Alert severity="error" variant="outlined" sx={{ mb: 2 }}>{error}</Alert>}
        {observationMessage && <Alert role="status" severity="info" variant="outlined" sx={{ mb: 2 }}>{observationMessage}</Alert>}
        {url && (
          <Alert role="status" severity="success" icon={<CheckCircleOutlined />} sx={{ mb: 2 }}>
            Draft PR opened: <Link href={url} target="_blank" rel="noopener noreferrer">{url}</Link>
          </Alert>
        )}

        {!preview && !url && (
          <Stack spacing={2.5}>
            {requestPresentation && !requestPresentation.canRegenerate && busy === null && !observationMessage && (
              <Alert
                severity={requestPresentation.severity}
                role={alertRole(requestPresentation.severity)}
                variant="outlined"
              >
                {requestPresentation.message}
              </Alert>
            )}
            <Alert role="status" severity="info" variant="outlined">
              {causeScope
                ? "Only the representative failed JUnit target for this cause, this cause-scoped response, its validated artifact evidence, server-verified immutable source identity, and your optional instruction are sent. The complete conversation is excluded."
                : exactAnalysis
                  ? "Only this exact failed JUnit analysis, this response, its validated artifact evidence, server-verified immutable source identity, and your optional instruction are sent. The complete conversation is excluded."
                  : "Only this response, its verified evidence, the selected recurring pattern, any enabled verified source finding, and your optional instruction are sent. The complete conversation is excluded."}
            </Alert>

            {!exactAnalysis && <ContextSection title="Recurring pattern" icon={<BuildOutlined sx={{ fontSize: 17, color: "warning.main" }} />}>
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
                <Box sx={{ borderRadius: 1, bgcolor: "action.selected", px: 1.5, py: 1.25 }}>
                  <Typography variant="body2" sx={{ fontWeight: 700 }}>{selectedPattern.subject}</Typography>
                  {selectedPattern.shared_root_cause && (
                    <Typography variant="body2" color="textSecondary" sx={{ mt: 0.55, lineHeight: 1.55 }}>
                      <RichText text={selectedPattern.shared_root_cause} steps />
                    </Typography>
                  )}
                  {selectedPattern.suggested_fix && (
                    <Typography variant="caption" color="primary" sx={{ display: "block", mt: 0.8, fontWeight: 700 }}>
                      Direction: {selectedPattern.suggested_fix}
                    </Typography>
                  )}
                  {selectedPattern.relevant_files && selectedPattern.relevant_files.length > 0 && (
                    <Box sx={{ mt: 1 }}>
                      <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700, mb: 0.45 }}>
                        Agent starting files
                      </Typography>
                      <Box
                        sx={{
                          borderRadius: 1,
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
            </ContextSection>}

            <ContextSection title="Selected chat finding" icon={<FactCheckOutlined sx={{ fontSize: 17, color: "success.main" }} />}>
              <Box sx={{ borderLeft: "1px solid", borderColor: "primary.main", pl: 1.5, py: 0.2 }}>
                <Typography variant="body2" sx={{ lineHeight: 1.6 }}>
                  <RichText text={message.content} steps />
                </Typography>
              </Box>
              {message.proposed_revision && (
                <Box sx={{ mt: 1.2, borderRadius: 1, bgcolor: "action.selected", p: 1.25 }}>
                  <Typography variant="caption" color="warning" sx={{ fontWeight: 750 }}>Evidence-backed revision</Typography>
                  <Typography variant="body2" sx={{ mt: 0.45 }}>{message.proposed_revision.root_cause}</Typography>
                  <Typography variant="body2" color="textSecondary" sx={{ mt: 0.45 }}>{message.proposed_revision.suggested_fix}</Typography>
                </Box>
              )}
              {message.citations && message.citations.length > 0 && (
                <Box sx={{ mt: 1.2 }}>
                  <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700, mb: 0.65 }}>
                    Verified artifact evidence
                  </Typography>
                  <EvidenceList citations={message.citations} />
                </Box>
              )}
              {request?.warning && !preview && (
                <Alert severity="warning" variant="outlined" sx={{ mt: 1.2 }}>
                  <Typography variant="caption" sx={{ display: "block", fontWeight: 750, mb: 0.35 }}>
                    Source verification warning
                  </Typography>
                  <Typography variant="body2">{request.warning}</Typography>
                </Alert>
              )}
            </ContextSection>

            {exactAnalysis && (
              <ContextSection title="Immutable source verification" icon={<FactCheckOutlined sx={{ fontSize: 17, color: "info.main" }} />}>
                <Alert role="status" severity="info" variant="outlined">
                  The server resolves the exact repository revision from build metadata, verifies the published source paths at that revision, and rejects the preview if the target branch has moved.
                </Alert>
              </ContextSection>
            )}

            {requestPresentation?.canRegenerate && busy === null && !observationMessage && (
              <Alert severity={requestPresentation.severity} role="status" variant="outlined">
                <Typography variant="body2" sx={{ fontWeight: 750, mb: 0.4 }}>
                  {isProviderCredentialRetry ? "Provider request refused" : "Generation completed without a patch"}
                </Typography>
                <Typography variant="body2">{requestPresentation.message}</Typography>
                {request?.failure?.operator_summary && (
                  <Box sx={{ mt: 1.1 }}>
                    <Typography variant="caption" sx={{ display: "block", fontWeight: 750, mb: 0.35 }}>
                      {isProviderCredentialRetry ? "Provider diagnostic" : "Coding agent summary"}
                    </Typography>
                    <Typography variant="body2" color="textSecondary">
                      {request.failure.operator_summary}
                    </Typography>
                  </Box>
                )}
              </Alert>
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
              helperText={instructionHelperText}
            />
            {requestPresentation?.canRegenerate && (
              <Button
                variant="outlined"
                onClick={() => void regeneratePreview()}
                disabled={busy !== null || (!isProviderCredentialRetry && !hasRevisedInstruction)}
                sx={{ alignSelf: "flex-start" }}
              >
                {busy === "regenerate"
                  ? isProviderCredentialRetry ? "Retrying" : "Regenerating"
                  : isProviderCredentialRetry ? "Retry fix preview" : "Regenerate with feedback"}
              </Button>
            )}
          </Stack>
        )}

        {(busy === "preview" || busy === "regenerate") && (
          <Stack direction="row" spacing={1.25} sx={{ alignItems: "center", py: 4, justifyContent: "center" }}>
            <CircularProgress size={20} />
            <Box>
              <Typography variant="body2" sx={{ fontWeight: 700 }}>
                {busy === "regenerate"
                  ? "Regenerating the fix preview"
                  : request?.stage === "drafting"
                    ? "Generating the fix preview"
                    : "Verifying the fix request"}
              </Typography>
              <Typography variant="caption" color="textSecondary">
                {exactAnalysis
                  ? "Generation continues in the background. You can close this dialog and return later."
                  : "The coding agent is using only the reviewed context."}
              </Typography>
            </Box>
          </Stack>
        )}

        {preview && !url && (
          <Stack spacing={2.25}>
            {request?.warning && (
              <Alert severity="warning" variant="outlined">
                <Typography variant="caption" sx={{ display: "block", fontWeight: 750, mb: 0.35 }}>
                  Source verification warning
                </Typography>
                <Typography variant="body2">{request.warning}</Typography>
              </Alert>
            )}
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
            {exactAnalysis && (
              <Stack spacing={1.25}>
                <TextField
                  label="Maintainer instruction"
                  placeholder="Describe the revision needed for the next preview"
                  fullWidth
                  multiline
                  minRows={2}
                  maxRows={5}
                  value={instruction}
                  onChange={(event) => setInstruction(limitChatFixInstruction(event.target.value))}
                  helperText={`${chatFixInstructionBytes(instruction)}/4096 bytes`}
                />
                <Button
                  variant="outlined"
                  onClick={() => void regeneratePreview()}
                  disabled={busy !== null || !instruction.trim() || instruction.trim() === submittedInstruction.trim()}
                  sx={{ alignSelf: "flex-start" }}
                >
                  {busy === "regenerate" ? "Regenerating" : "Regenerate with feedback"}
                </Button>
              </Stack>
            )}
          </Stack>
        )}
      </DialogContent>

      <DialogActions sx={{ px: dialogGutter, py: 2 }}>
        <Button color="inherit" onClick={close} disabled={busy !== null && !(exactAnalysis && busy === "preview")}>
          {url ? "Done" : exactAnalysis && busy === "preview" ? "Close" : "Cancel"}
        </Button>
        {!request && !preview && !url && (
          <Button
            variant="contained"
            startIcon={busy === "preview" ? <CircularProgress size={16} color="inherit" /> : <BuildOutlined />}
            onClick={() => void generatePreview()}
            disabled={busy !== null || (!exactAnalysis && !patternID)}
          >
            {busy === "preview" ? "Generating" : "Generate fix preview"}
          </Button>
        )}
        {request?.status === "ready" && request.preview && !preview && !url && (
          <Button
            variant="contained"
            startIcon={<BuildOutlined />}
            onClick={() => setPreview(request.preview ?? null)}
            disabled={busy !== null}
          >
            Review fix preview
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
            {busy === "confirm"
              ? "Opening draft PR"
              : request?.warning
                ? "Open draft PR with warnings"
                : "Open draft PR"}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
