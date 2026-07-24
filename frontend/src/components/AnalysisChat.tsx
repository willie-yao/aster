import { useEffect, useMemo, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Divider from "@mui/material/Divider";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import {
  ArrowUpward,
  AutoAwesome,
  ExpandMore,
  FactCheckOutlined,
  HelpOutlined,
  PsychologyAltOutlined,
  PublishedWithChangesOutlined,
  ReportProblemOutlined,
  StopCircleOutlined,
} from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import {
  analysisChatActiveTurnLimitMessage,
  analysisChatIdempotencyConflictMessage,
  analysisChatRateLimitMessage,
  analysisChatRequestOutcomeUnknownMessage,
  analysisChatRequestPendingMessage,
  analysisChatSessionBusyMessage,
  analysisChatTurnLimitMessage,
  AnalysisChatAPIError,
  cancelAnalysisChatRequest,
  createAnalysisChatSession,
  getAnalysisChatSession,
  isAmbiguousAnalysisChatFailure,
  limitAnalysisChatQuestion,
  newAnalysisChatRequestID,
  streamAnalysisChatMessage,
} from "../lib/analysisChat";
import { fileToUrl, type FileToUrlContext } from "../lib/utils";
import { confirmAnalysisCorrection, previewAnalysisCorrection } from "../lib/analysisCorrections";
import { soft } from "../theme";
import type {
  AnalysisChatAssessment,
  AnalysisChatCitation,
  AnalysisChatMessage,
  AnalysisChatProgressPhase,
  AnalysisChatReference,
  AnalysisChatSession,
} from "../types/analysisChat";
import { RichText } from "./RichText";
import { AnalysisCorrectionDialog } from "./AnalysisCorrectionDialog";
import type { AnalysisCorrectionPreview } from "../types/corrections";

interface PendingTurn {
  sessionID: string;
  requestID: string;
  question: string;
}

const suggestedQuestions = [
  "What evidence supports this conclusion?",
  "What would disprove this root cause?",
  "Could this failure be transient?",
  "Check a different hypothesis",
] as const;

const assessmentConfig: Record<
  AnalysisChatAssessment,
  { label: string; color: "primary" | "success" | "warning" | "default" }
> = {
  explains: { label: "Explains analysis", color: "primary" },
  supports: { label: "Evidence supports it", color: "success" },
  challenges: { label: "Evidence challenges it", color: "warning" },
  inconclusive: { label: "Evidence inconclusive", color: "default" },
};

function readableError(error: unknown): string {
  if (error instanceof AnalysisChatAPIError) {
    switch (error.status) {
      case 404:
        return "This analysis or conversation is no longer available. Refresh the page to load the latest data.";
      case 409:
        if (error.message === analysisChatSessionBusyMessage || error.message === analysisChatRequestPendingMessage) {
          return "Another answer is still running for this conversation. Retry shortly to reconnect.";
        }
        if (error.message === analysisChatRequestOutcomeUnknownMessage) {
          return "The previous answer could not be confirmed after a server interruption. Submit again to retry.";
        }
        if (error.message === analysisChatIdempotencyConflictMessage) {
          return "This request changed while it was being retried. Refresh the page before continuing.";
        }
        return "The published analysis changed while this page was open. Refresh before starting a new conversation.";
      case 429:
        if (error.message === analysisChatTurnLimitMessage) {
          return "This conversation reached its limit. Start again from the latest analysis.";
        }
        if (error.message === analysisChatActiveTurnLimitMessage) {
          return "You already have the maximum number of active analysis turns. Wait for one to finish.";
        }
        if (error.message === analysisChatRateLimitMessage) {
          return "Too many analysis questions were started recently. Try again in a minute.";
        }
        return "The analysis chat service is at capacity. Try again later.";
      case 499:
        return "The analysis request was cancelled.";
      case 504:
        return "The analysis agent timed out before it could answer. Try a narrower question.";
      default:
        return error.message;
    }
  }
  if (error instanceof Error && error.name !== "AbortError") return error.message;
  return "The analysis agent could not complete the request.";
}

function formatLines(citation: AnalysisChatCitation) {
  if (!citation.line_start) return "";
  if (!citation.line_end || citation.line_end === citation.line_start) {
    return `line ${citation.line_start}`;
  }
  return `lines ${citation.line_start}-${citation.line_end}`;
}

function AssistantMessage({
  message,
  fileCtx,
  correctionEnabled,
  onReviewCorrection,
}: {
  message: AnalysisChatMessage;
  fileCtx: FileToUrlContext;
  correctionEnabled: boolean;
  onReviewCorrection: (requestID: string) => void;
}) {
  const assessment = message.assessment
    ? assessmentConfig[message.assessment]
    : assessmentConfig.explains;
  return (
    <Box
      sx={{
        border: "1px solid",
        borderColor: (theme) => soft(theme, assessment.color === "default" ? "primary" : assessment.color, 0.24),
        borderRadius: "12px",
        bgcolor: (theme) => soft(theme, assessment.color === "default" ? "primary" : assessment.color, 0.045),
        overflow: "hidden",
      }}
    >
      <Stack
        direction="row"
        spacing={1}
        sx={{ alignItems: "center", px: 1.5, py: 1, borderBottom: "1px solid", borderColor: "divider" }}
      >
        <AutoAwesome sx={{ color: "primary.main", fontSize: 17 }} />
        <Typography variant="label" sx={{ fontWeight: 700, color: "text.primary" }}>
          Analysis agent
        </Typography>
        <Chip
          size="small"
          color={assessment.color}
          variant="outlined"
          label={assessment.label}
          sx={{ ml: "auto", height: 24, fontSize: "0.68rem" }}
        />
      </Stack>
      <Stack spacing={1.5} sx={{ p: 1.5 }}>
        <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.65 }}>
          <RichText text={message.content} steps fileCtx={fileCtx} />
        </Typography>

        {message.citations && message.citations.length > 0 && (
          <Box>
            <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", mb: 0.75 }}>
              <FactCheckOutlined sx={{ fontSize: 16, color: "success.main" }} />
              <Typography variant="label" color="text.secondary" sx={{ fontWeight: 700 }}>
                Evidence read this turn
              </Typography>
            </Stack>
            <Stack spacing={0.75}>
              {message.citations.map((citation, index) => {
                const url = fileToUrl(citation.path, fileCtx);
                const lines = formatLines(citation);
                return (
                  <Box
                    key={`${citation.path}-${citation.line_start ?? 0}-${index}`}
                    sx={{
                      borderLeft: "2px solid",
                      borderColor: "success.main",
                      pl: 1.25,
                      py: 0.25,
                    }}
                  >
                    <Stack direction="row" spacing={0.75} sx={{ alignItems: "baseline", flexWrap: "wrap" }}>
                      {url ? (
                        <Link
                          href={url}
                          target="_blank"
                          rel="noopener noreferrer"
                          sx={{ fontFamily: "monospace", fontSize: "0.75rem", fontWeight: 650 }}
                        >
                          {citation.path}
                        </Link>
                      ) : (
                        <Typography component="span" sx={{ fontFamily: "monospace", fontSize: "0.75rem" }}>
                          {citation.path}
                        </Typography>
                      )}
                      {lines && (
                        <Typography component="span" variant="caption" color="text.secondary">
                          {lines}
                        </Typography>
                      )}
                    </Stack>
                    {citation.quote && (
                      <Typography
                        component="blockquote"
                        variant="caption"
                        color="text.secondary"
                        sx={{ m: 0, mt: 0.35, fontFamily: "monospace", lineHeight: 1.55 }}
                      >
                        “{citation.quote}”
                      </Typography>
                    )}
                  </Box>
                );
              })}
            </Stack>
          </Box>
        )}

        {message.proposed_revision && (
          <Box
            sx={{
              borderRadius: "10px",
              border: "1px solid",
              borderColor: (theme) => soft(theme, "warning", 0.35),
              bgcolor: (theme) => soft(theme, "warning", 0.07),
              p: 1.5,
            }}
          >
            <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", mb: 1 }}>
              <ReportProblemOutlined sx={{ color: "warning.main", fontSize: 17 }} />
              <Typography variant="label" sx={{ fontWeight: 750 }}>
                Proposed revision
              </Typography>
              <Chip size="small" label="Not published" color="warning" variant="outlined" sx={{ ml: "auto", height: 22 }} />
            </Stack>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 700 }}>
              Revised root cause
            </Typography>
            <Typography variant="body2" sx={{ mt: 0.25, lineHeight: 1.6 }}>
              <RichText text={message.proposed_revision.root_cause} steps fileCtx={fileCtx} />
            </Typography>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 700, mt: 1.25 }}>
              Revised fix
            </Typography>
            <Typography variant="body2" sx={{ mt: 0.25, lineHeight: 1.6 }}>
              <RichText text={message.proposed_revision.suggested_fix} steps fileCtx={fileCtx} />
            </Typography>
            {correctionEnabled && message.request_id && (
              <Button
                size="small"
                variant="outlined"
                color="warning"
                startIcon={<PublishedWithChangesOutlined />}
                onClick={() => onReviewCorrection(message.request_id!)}
                sx={{ mt: 1.5 }}
              >
                Review correction
              </Button>
            )}
          </Box>
        )}
      </Stack>
    </Box>
  );
}

const progressLabels: Record<AnalysisChatProgressPhase, { title: string; detail: string }> = {
  queued: { title: "Queued", detail: "Waiting for an analysis turn to start." },
  investigating: { title: "Investigating the analysis", detail: "Reviewing the published conclusion and failure context." },
  reading_evidence: { title: "Reading build evidence", detail: "Reopening relevant artifacts before answering." },
  evaluating: { title: "Evaluating the evidence", detail: "Comparing the artifacts with the published conclusion." },
  finalizing: { title: "Finalizing the answer", detail: "Validating the response and its citations." },
  cancelling: { title: "Cancelling the request", detail: "Stopping the active analysis turn." },
};

function ThinkingState({
  phase,
  cancelling,
  onCancel,
}: {
  phase: AnalysisChatProgressPhase;
  cancelling: boolean;
  onCancel: () => void;
}) {
  const copy = progressLabels[phase];
  return (
    <Stack
      role="status"
      aria-live="polite"
      direction="row"
      spacing={1.25}
      sx={{
        alignItems: "center",
        borderRadius: "10px",
        px: 1.5,
        py: 1.25,
        bgcolor: (theme) => soft(theme, "primary", 0.055),
      }}
    >
      <Stack direction="row" spacing={0.4} aria-hidden="true">
        {[0, 1, 2].map((i) => (
          <Box
            key={i}
            sx={{
              width: 5,
              height: 5,
              borderRadius: "50%",
              bgcolor: "primary.main",
              animation: "analysisChatPulse 1.2s ease-in-out infinite",
              animationDelay: `${i * 150}ms`,
              "@keyframes analysisChatPulse": {
                "0%, 70%, 100%": { opacity: 0.25, transform: "translateY(0)" },
                "35%": { opacity: 1, transform: "translateY(-3px)" },
              },
            }}
          />
        ))}
      </Stack>
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 650 }}>
          {copy.title}
        </Typography>
        <Typography variant="caption" color="text.secondary">
          {copy.detail}
        </Typography>
      </Box>
      <Button
        size="small"
        variant="outlined"
        color="inherit"
        startIcon={<StopCircleOutlined />}
        onClick={onCancel}
        disabled={cancelling}
        sx={{ flexShrink: 0 }}
      >
        {cancelling ? "Cancelling" : "Cancel"}
      </Button>
    </Stack>
  );
}

export function AnalysisChat({
  analysisRef,
  fileCtx,
  onCorrectionChanged,
}: {
  analysisRef: AnalysisChatReference;
  fileCtx: FileToUrlContext;
  onCorrectionChanged?: () => void;
}) {
  const { features } = useCapabilities();
  const auth = useAuth();
  const [expanded, setExpanded] = useState(false);
  const [question, setQuestion] = useState("");
  const [session, setSession] = useState<AnalysisChatSession | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [turnLimitExhausted, setTurnLimitExhausted] = useState(false);
  const [pendingTurn, setPendingTurn] = useState<PendingTurn | null>(null);
  const [progressPhase, setProgressPhase] = useState<AnalysisChatProgressPhase>("queued");
  const [cancelling, setCancelling] = useState(false);
  const [correctionPreview, setCorrectionPreview] = useState<AnalysisCorrectionPreview | null>(null);
  const [correctionOpen, setCorrectionOpen] = useState(false);
  const [correctionBusy, setCorrectionBusy] = useState(false);
  const createRequestIDRef = useRef(newAnalysisChatRequestID());
  const controllerRef = useRef<AbortController | null>(null);
  const cancelControllerRef = useRef<AbortController | null>(null);
  const identityRef = useRef("");
  const endRef = useRef<HTMLDivElement | null>(null);

  const identity = useMemo(
    () =>
      [
        analysisRef.job_id,
        analysisRef.build_id,
        analysisRef.test_name,
        analysisRef.suite_name,
        analysisRef.class_name,
        analysisRef.junit_file,
        analysisRef.analysis_generated_at,
      ].join("\u0000"),
    [analysisRef],
  );
  identityRef.current = identity;

  useEffect(() => {
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
    setExpanded(false);
    setQuestion("");
    setSession(null);
    setBusy(false);
    setError(null);
    setTurnLimitExhausted(false);
    setPendingTurn(null);
    setProgressPhase("queued");
    setCancelling(false);
    setCorrectionPreview(null);
    setCorrectionOpen(false);
    setCorrectionBusy(false);
    createRequestIDRef.current = newAnalysisChatRequestID();
  }, [identity]);

  useEffect(() => {
    if (expanded && (session?.messages.length || busy)) {
      endRef.current?.scrollIntoView({ behavior: "smooth", block: "nearest" });
    }
  }, [busy, expanded, session?.messages.length]);

  useEffect(() => () => {
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
  }, []);

  if (!features.analysis_chat) return null;

  const userTurns = session?.messages.filter((message) => message.role === "user").length ?? 0;
  const turnLimitReached = turnLimitExhausted || userTurns >= 10;

  async function submit(nextQuestion?: string) {
    const value = (nextQuestion ?? pendingTurn?.question ?? question).trim();
    if (!value || busy || turnLimitReached) return;
    if (pendingTurn && pendingTurn.question !== value) {
      setError("The previous question may still be running. Retry it before asking another question.");
      return;
    }
    if (auth.status === "anonymous") {
      auth.signIn();
      return;
    }
    if (auth.status !== "authenticated") return;

    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy(true);
    setError(null);
    let activeSession = session;
    let activeTurn = pendingTurn;
    try {
      if (!activeSession) {
        activeSession = await createAnalysisChatSession(
          analysisRef,
          createRequestIDRef.current,
          controller.signal,
        );
        setSession(activeSession);
      }
      if (!activeTurn) {
        activeTurn = {
          sessionID: activeSession.id,
          requestID: newAnalysisChatRequestID(),
          question: value,
        };
        setPendingTurn(activeTurn);
        setQuestion(value);
      }
      setProgressPhase("queued");
      const updated = await streamAnalysisChatMessage(
        activeTurn.sessionID,
        activeTurn.question,
        activeTurn.requestID,
        (progress) => setProgressPhase(progress.phase),
        controller.signal,
      );
      setSession(updated);
      setQuestion("");
      setPendingTurn(null);
    } catch (requestError) {
      if (requestError instanceof Error && requestError.name === "AbortError") return;
      if (
        requestError instanceof AnalysisChatAPIError &&
        requestError.status === 401 &&
        auth.mode === "oauth"
      ) {
        auth.signIn();
        return;
      }

      const ambiguousFailure = isAmbiguousAnalysisChatFailure(requestError);
      if (activeSession && activeTurn && ambiguousFailure) {
        try {
          const reconciled = await getAnalysisChatSession(activeSession.id, controller.signal);
          setSession(reconciled);
          if (reconciled.messages.some((message) => message.request_id === activeTurn?.requestID)) {
            setQuestion("");
            setPendingTurn(null);
            return;
          }
        } catch (reconcileError) {
          if (reconcileError instanceof Error && reconcileError.name === "AbortError") return;
          if (
            reconcileError instanceof AnalysisChatAPIError &&
            reconcileError.status === 401 &&
            auth.mode === "oauth"
          ) {
            auth.signIn();
            return;
          }
        }
        setPendingTurn(activeTurn);
        setError("The question may still be running. Retry shortly to reconnect to the same request.");
        return;
      }
      if (!activeSession && ambiguousFailure) {
        setError("The conversation may have been created. Retry to reconnect to the same session.");
        return;
      }

      const exhausted =
        requestError instanceof AnalysisChatAPIError &&
        requestError.status === 429 &&
        requestError.message === analysisChatTurnLimitMessage;
      if (exhausted) {
        setTurnLimitExhausted(true);
        setPendingTurn(null);
        setError(null);
      } else {
        const stillRunning =
          requestError instanceof AnalysisChatAPIError &&
          requestError.status === 409 &&
          (requestError.message === analysisChatSessionBusyMessage ||
            requestError.message === analysisChatRequestPendingMessage);
        if (!stillRunning) setPendingTurn(null);
        setError(readableError(requestError));
      }
    } finally {
      if (controllerRef.current === controller) {
        controllerRef.current = null;
        setBusy(false);
        setCancelling(false);
      }
    }
  }

  async function reviewCorrection(requestID: string) {
    if (!session) return;
    setCorrectionBusy(true);
    setError(null);
    try {
      const preview = await previewAnalysisCorrection(session.id, requestID);
      setCorrectionPreview(preview);
      setCorrectionOpen(true);
    } catch (previewError) {
      setError(previewError instanceof Error ? previewError.message : "Could not prepare the correction.");
    } finally {
      setCorrectionBusy(false);
    }
  }

  async function publishCorrection() {
    if (!correctionPreview) return;
    setCorrectionBusy(true);
    try {
      await confirmAnalysisCorrection(correctionPreview.token);
      setCorrectionOpen(false);
      setCorrectionPreview(null);
      onCorrectionChanged?.();
    } catch (confirmError) {
      setError(confirmError instanceof Error ? confirmError.message : "Could not publish the correction.");
    } finally {
      setCorrectionBusy(false);
    }
  }

  async function cancelTurn() {
    if (!pendingTurn || cancelling) return;
    const cancelIdentity = identity;
    const turn = pendingTurn;
    cancelControllerRef.current?.abort();
    const controller = new AbortController();
    cancelControllerRef.current = controller;
    setCancelling(true);
    setProgressPhase("cancelling");
    try {
      await cancelAnalysisChatRequest(turn.sessionID, turn.requestID, controller.signal);
      if (identityRef.current !== cancelIdentity) return;
      if (!busy) await submit(turn.question);
    } catch (cancelError) {
      if (cancelError instanceof Error && cancelError.name === "AbortError") return;
      if (identityRef.current !== cancelIdentity) return;
      setError(readableError(cancelError));
    } finally {
      if (cancelControllerRef.current === controller) {
        cancelControllerRef.current = null;
        if (identityRef.current === cancelIdentity) setCancelling(false);
      }
    }
  }

  function toggleChat() {
    if (auth.status === "anonymous") {
      auth.signIn();
      return;
    }
    setExpanded((value) => !value);
  }

  return (
    <Box sx={{ mt: 0.5 }}>
      <Divider sx={{ mb: 1.5 }} />
      <Box
        sx={{
          borderRadius: "14px",
          border: "1px solid",
          borderColor: (theme) => soft(theme, "primary", 0.3),
          bgcolor: (theme) => soft(theme, "primary", 0.025),
          overflow: "hidden",
        }}
      >
        <Stack
          direction="row"
          spacing={0.25}
          sx={{
            alignItems: "center",
            px: 1,
            py: 0.5,
            borderBottom: expanded ? "1px solid" : 0,
            borderColor: "divider",
          }}
        >
          <ButtonBase
            disableRipple
            onClick={toggleChat}
            disabled={auth.status === "loading" || auth.status === "unavailable"}
            aria-expanded={expanded}
            aria-controls="analysis-chat-content"
            sx={{
              minWidth: 0,
              flex: 1,
              justifyContent: "flex-start",
              gap: 1,
              borderRadius: "10px",
              px: 0.5,
              py: 0.75,
              textAlign: "left",
              "&.Mui-disabled": { opacity: 0.5 },
            }}
          >
            <Box
              sx={{
                width: 30,
                height: 30,
                display: "grid",
                placeItems: "center",
                borderRadius: "9px",
                bgcolor: (theme) => soft(theme, "primary", 0.14),
                color: "primary.main",
                flexShrink: 0,
              }}
            >
              <PsychologyAltOutlined sx={{ fontSize: 19 }} />
            </Box>
            <Typography variant="body2" sx={{ fontWeight: 750 }}>
              Chat with agent
            </Typography>
          </ButtonBase>
          <Tooltip title="This conversation does not change the published analysis">
            <IconButton
              disableRipple
              size="small"
              aria-label="This conversation does not change the published analysis"
              sx={{ p: 0.5 }}
            >
              <HelpOutlined sx={{ color: "text.secondary", fontSize: 17 }} />
            </IconButton>
          </Tooltip>
          <IconButton
            disableRipple
            size="small"
            aria-label={expanded ? "Collapse analysis chat" : "Expand analysis chat"}
            aria-expanded={expanded}
            aria-controls="analysis-chat-content"
            onClick={toggleChat}
            disabled={auth.status === "loading" || auth.status === "unavailable"}
          >
            <ExpandMore
              fontSize="small"
              sx={{
                transition: (theme) =>
                  theme.transitions.create("transform", { duration: theme.transitions.duration.short }),
                transform: expanded ? "rotate(180deg)" : "rotate(0deg)",
              }}
            />
          </IconButton>
        </Stack>

        <Collapse in={expanded} appear>
          <Box id="analysis-chat-content">
            <Stack
              spacing={1.25}
              aria-live="polite"
              sx={{ p: { xs: 1.25, sm: 1.5 }, maxHeight: 520, overflowY: "auto" }}
            >
              {!session?.messages.length && !busy && !pendingTurn && !turnLimitReached && (
                <Box sx={{ py: 0.5 }}>
                  <Typography variant="body2" sx={{ fontWeight: 650 }}>
                    Interrogate the conclusion, not just the summary.
                  </Typography>
                  <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.35, mb: 1.25 }}>
                    Ask for evidence, test another cause, or challenge what the agent missed.
                  </Typography>
                  <Stack direction="row" spacing={0.75} useFlexGap sx={{ flexWrap: "wrap" }}>
                    {suggestedQuestions.map((suggestion) => (
                      <Chip
                        key={suggestion}
                        label={suggestion}
                        onClick={() => void submit(suggestion)}
                        icon={<PsychologyAltOutlined />}
                        variant="outlined"
                        sx={{
                          height: "auto",
                          minHeight: 30,
                          borderColor: "divider",
                          "& .MuiChip-label": { whiteSpace: "normal", py: 0.55, fontSize: "0.72rem" },
                          "& .MuiChip-icon": { fontSize: 15 },
                        }}
                      />
                    ))}
                  </Stack>
                </Box>
              )}

              {session?.messages.map((message, index) =>
                message.role === "user" ? (
                  <Box
                    key={`${message.created_at}-${index}`}
                    sx={{
                      ml: { xs: 2, sm: 5 },
                      borderRadius: "10px 10px 3px 10px",
                      bgcolor: (theme) => soft(theme, "primary", 0.12),
                      border: "1px solid",
                      borderColor: (theme) => soft(theme, "primary", 0.22),
                      px: 1.5,
                      py: 1.1,
                    }}
                  >
                    <Typography variant="body2" sx={{ lineHeight: 1.55 }}>
                      {message.content}
                    </Typography>
                  </Box>
                ) : (
                  <AssistantMessage
                    key={`${message.created_at}-${index}`}
                    message={message}
                    fileCtx={fileCtx}
                    correctionEnabled={Boolean(features.analysis_corrections)}
                    onReviewCorrection={(requestID) => void reviewCorrection(requestID)}
                  />
                ),
              )}

              {busy && pendingTurn && (
                <ThinkingState
                  phase={progressPhase}
                  cancelling={cancelling}
                  onCancel={() => void cancelTurn()}
                />
              )}
              {error && <Alert severity="error" variant="outlined">{error}</Alert>}
              <div ref={endRef} />
            </Stack>

            <Box sx={{ px: { xs: 1.25, sm: 1.5 }, pb: 1.5 }}>
              {turnLimitReached ? (
                <Alert severity="info" variant="outlined">
                  This conversation reached its ten-turn limit.
                </Alert>
              ) : (
                <Stack direction="row" spacing={0.75} sx={{ alignItems: "center" }}>
                  <TextField
                    fullWidth
                    multiline
                    minRows={1}
                    maxRows={5}
                    value={question}
                    onChange={(event) => setQuestion(limitAnalysisChatQuestion(event.target.value))}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
                        event.preventDefault();
                        void submit();
                      }
                    }}
                    disabled={busy || pendingTurn !== null}
                    placeholder="Ask why, challenge the cause, or test another hypothesis..."
                    slotProps={{
                      input: {
                        sx: {
                          borderRadius: "10px",
                          bgcolor: "background.paper",
                          fontSize: "0.875rem",
                        },
                      },
                      htmlInput: { "aria-label": "Ask about this analysis" },
                    }}
                  />
                  <Tooltip title={pendingTurn ? "Retry question" : "Send question"}>
                    <span>
                      <IconButton
                        color="primary"
                        aria-label={pendingTurn ? "Retry question" : "Send question"}
                        onClick={() => void submit()}
                        disabled={busy || (pendingTurn?.question ?? question).trim() === ""}
                        sx={{
                          width: 48,
                          height: 48,
                          borderRadius: "10px",
                          bgcolor: "primary.main",
                          color: "primary.contrastText",
                          "&:hover": { bgcolor: "primary.dark" },
                          "&.Mui-disabled": { bgcolor: "action.disabledBackground" },
                        }}
                      >
                        <ArrowUpward fontSize="small" />
                      </IconButton>
                    </span>
                  </Tooltip>
                  {pendingTurn && !busy && (
                    <Tooltip title="Cancel pending question">
                      <span>
                        <IconButton
                          aria-label="Cancel pending question"
                          onClick={() => void cancelTurn()}
                          disabled={cancelling}
                          sx={{
                            width: 48,
                            height: 48,
                            borderRadius: "10px",
                            border: "1px solid",
                            borderColor: "divider",
                            color: "text.secondary",
                          }}
                        >
                          <StopCircleOutlined fontSize="small" />
                        </IconButton>
                      </span>
                    </Tooltip>
                  )}
                </Stack>
              )}
              {session && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  sx={{ display: "block", mt: 0.75, textAlign: "right" }}
                >
                  {turnLimitExhausted ? "10/10 attempts" : `${userTurns}/10 turns`}
                </Typography>
              )}
            </Box>
          </Box>
        </Collapse>
      </Box>
      <AnalysisCorrectionDialog
        preview={correctionPreview}
        open={correctionOpen}
        busy={correctionBusy}
        onClose={() => setCorrectionOpen(false)}
        onConfirm={() => void publishCorrection()}
      />
    </Box>
  );
}
