import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  BuildOutlined,
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
  analysisChatAttemptStatus,
  analysisChatFailureGuidance,
  analysisChatHistory,
  analysisChatIdempotencyConflictMessage,
  analysisChatRateLimitMessage,
  analysisChatRequestOutcomeUnknownMessage,
  analysisChatRequestPendingMessage,
  analysisChatRequestState,
  analysisChatSessionBusyMessage,
  analysisChatTurnLimitReached,
  analysisChatProgressTurnUsage,
  analysisChatTurnLimitMessage,
  analysisChatTurnUsage,
  AnalysisChatAPIError,
  cancelAnalysisChatRequest,
  createAnalysisChatSession,
  findAnalysisChatSession,
  getAnalysisChatSession,
  isAmbiguousAnalysisChatFailure,
  limitAnalysisChatQuestion,
  markAnalysisChatTurnLimitReached,
  newAnalysisChatRequestID,
  resumeAnalysisChatTurn,
  streamAnalysisChatMessage,
} from "../lib/analysisChat";
import { fileToUrl, type FileToUrlContext } from "../lib/utils";
import { AnalysisCorrectionAPIError, confirmAnalysisCorrection, previewAnalysisCorrection } from "../lib/analysisCorrections";
import { soft } from "../theme";
import type {
  AnalysisChatAssessment,
  AnalysisChatAttempt,
  AnalysisChatCitation,
  AnalysisChatMessage,
  AnalysisChatProgress,
  AnalysisChatProgressPhase,
  AnalysisChatReference,
  AnalysisChatSession,
} from "../types/analysisChat";
import { RichText } from "./RichText";
import { AnalysisCorrectionDialog } from "./AnalysisCorrectionDialog";
import { SourceInvestigationPanel } from "./SourceInvestigationPanel";
import type { AnalysisCorrectionPreview } from "../types/corrections";
import type { PatternAnalysis } from "../types/dashboard";
import type { SourceInvestigationView } from "../types/sourceInvestigation";
import { ChatFixDialog, type ChatFixSourceSelection } from "./ChatFixDialog";

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

const patternSuggestedQuestions = [
  "Which affected builds provide the strongest evidence?",
  "What would disprove this shared root cause?",
  "Do the failures differ across builds?",
  "Check a different cross-build hypothesis",
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
  const guidance = analysisChatFailureGuidance(error);
  if (guidance) return guidance;
  if (error instanceof AnalysisChatAPIError) {
    switch (error.status) {
      case 404:
        return "This analysis or conversation is no longer available. Refresh the page to load the latest data.";
      case 409:
        if (error.message === analysisChatSessionBusyMessage || error.message === analysisChatRequestPendingMessage) {
          return "Another answer is still running for this conversation. Select Continue to reconnect.";
        }
        if (error.message === analysisChatRequestOutcomeUnknownMessage) {
          return "The previous answer could not be confirmed after a server interruption. Select Continue to try again.";
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

function UserMessage({ content }: { content: string }) {
  return (
    <Box
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
        {content}
      </Typography>
    </Box>
  );
}

function AttemptSummary({ attempt }: { attempt: AnalysisChatAttempt }) {
  const status = analysisChatAttemptStatus(attempt);
  const severity = attempt.outcome === "succeeded"
    ? "success"
    : attempt.outcome === "pending"
      ? "info"
      : attempt.outcome === "cancelled" || attempt.outcome === "unknown"
        ? "warning"
        : "error";
  return (
    <Stack spacing={0.75}>
      {attempt.question
        ? <UserMessage content={attempt.question} />
        : (
          <Typography variant="caption" color="text.secondary" sx={{ ml: { xs: 2, sm: 5 } }}>
            Question text is unavailable for this earlier attempt.
          </Typography>
        )}
      <Alert severity={severity} variant="outlined" sx={{ py: 0.25 }}>
        <Typography variant="body2" sx={{ fontWeight: 700 }}>{status.label}</Typography>
        <Typography variant="caption" color="text.secondary">{status.detail}</Typography>
      </Alert>
    </Stack>
  );
}

function AssistantMessage({
  message,
  fileCtx,
  correctionEnabled,
  sourceInvestigationEnabled,
  chatFixEnabled,
  fixEligible,
  sessionID,
  sourceRepository,
  onReviewCorrection,
  onUseForFix,
  onSourceInvestigationChange,
}: {
  message: AnalysisChatMessage;
  fileCtx: FileToUrlContext;
  correctionEnabled: boolean;
  sourceInvestigationEnabled: boolean;
  chatFixEnabled: boolean;
  fixEligible: boolean;
  sessionID: string;
  sourceRepository?: { owner: string; name: string; revision: string };
  onReviewCorrection: (requestID: string) => void;
  onUseForFix: () => void;
  onSourceInvestigationChange: (requestID: string | null, view: SourceInvestigationView | null) => void;
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

        {sourceInvestigationEnabled && message.request_id && (
          <SourceInvestigationPanel
            sessionID={sessionID}
            chatRequestID={message.request_id}
            repository={sourceRepository}
            onInvestigationChange={onSourceInvestigationChange}
          />
        )}

        {chatFixEnabled && fixEligible && message.request_id && (
          <Button
            size="small"
            variant="outlined"
            color="warning"
            startIcon={<BuildOutlined />}
            onClick={onUseForFix}
            sx={{ alignSelf: "flex-start" }}
          >
            Use this finding in a fix proposal
          </Button>
        )}
      </Stack>
    </Box>
  );
}

const progressLabels: Record<AnalysisChatProgressPhase, { title: string; detail: string }> = {
  queued: { title: "Investigating", detail: "Waiting for the analysis turn to start." },
  investigating: { title: "Investigating", detail: "Reviewing the published conclusion and failure context." },
  reading_evidence: { title: "Validating evidence", detail: "Reading the artifacts needed for this answer." },
  evaluating: { title: "Investigating", detail: "Comparing the evidence with the published conclusion." },
  finalizing: { title: "Finalizing", detail: "Checking the response and its citations." },
  validation_retrying: { title: "Validating evidence", detail: "The response contract was rejected and is being retried." },
  cancelling: { title: "Cancelling", detail: "Stopping the active analysis turn." },
};

function ThinkingState({
  phase, cancelling, startedAt, validationRetries, maxValidationRetries, onCancel,
}: {
  phase: AnalysisChatProgressPhase;
  cancelling: boolean;
  startedAt?: string;
  validationRetries: number;
  maxValidationRetries: number;
  onCancel: () => void;
}) {
  const copy = progressLabels[phase];
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  const started = startedAt ? Date.parse(startedAt) : Number.NaN;
  const elapsed = Number.isFinite(started) ? Math.max(0, Math.floor((now - started) / 1000)) : null;
  return (
    <Stack role="status" aria-live="polite" direction="row" spacing={1.25} sx={{
      alignItems: "center", borderRadius: "10px", px: 1.5, py: 1.25,
      bgcolor: (theme) => soft(theme, "primary", 0.055),
    }}>
      <Stack direction="row" spacing={0.4} aria-hidden="true">
        {[0, 1, 2].map((i) => (
          <Box key={i} sx={{
            width: 5, height: 5, borderRadius: "50%", bgcolor: "primary.main",
            animation: "analysisChatPulse 1.2s ease-in-out infinite", animationDelay: `${i * 150}ms`,
            "@keyframes analysisChatPulse": {
              "0%, 70%, 100%": { opacity: 0.25, transform: "translateY(0)" },
              "35%": { opacity: 1, transform: "translateY(-3px)" },
            },
          }} />
        ))}
      </Stack>
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 650 }}>{copy.title}</Typography>
        <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>{copy.detail}</Typography>
        <Typography variant="caption" color="text.secondary">
          {elapsed !== null ? `${elapsed}s elapsed` : "Elapsed time unavailable"}
          {phase === "validation_retrying" && maxValidationRetries > 0
            ? ` · Validation retry ${validationRetries} of ${maxValidationRetries}` : ""}
        </Typography>
      </Box>
      <Button size="small" variant="outlined" color="inherit" startIcon={<StopCircleOutlined />}
        onClick={onCancel} disabled={cancelling} sx={{ flexShrink: 0 }}>
        {cancelling ? "Cancelling" : "Cancel"}
      </Button>
    </Stack>
  );
}

export function AnalysisChat({
  analysisRef,
  fileCtx,
  fixPatterns = [],
  onCorrectionChanged,
}: {
  analysisRef: AnalysisChatReference;
  fileCtx: FileToUrlContext;
  fixPatterns?: PatternAnalysis[];
  onCorrectionChanged?: () => void;
}) {
  const { features } = useCapabilities();
  const auth = useAuth();
  const [expanded, setExpanded] = useState(false);
  const [question, setQuestion] = useState("");
  const [session, setSession] = useState<AnalysisChatSession | null>(null);
  const [restoring, setRestoring] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [turnLimitRejected, setTurnLimitRejected] = useState(false);
  const [pendingTurn, setPendingTurn] = useState<PendingTurn | null>(null);
  const [continueMode, setContinueMode] = useState(false);
  const [progressPhase, setProgressPhase] = useState<AnalysisChatProgressPhase>("queued");
  const [progressStartedAt, setProgressStartedAt] = useState<string | undefined>();
  const [validationRetries, setValidationRetries] = useState(0);
  const [maxValidationRetries, setMaxValidationRetries] = useState(0);
  const [cancelling, setCancelling] = useState(false);
  const [correctionPreview, setCorrectionPreview] = useState<AnalysisCorrectionPreview | null>(null);
  const [correctionOpen, setCorrectionOpen] = useState(false);
  const [correctionBusy, setCorrectionBusy] = useState(false);
  const [correctionError, setCorrectionError] = useState<string | null>(null);
  const [fixMessage, setFixMessage] = useState<AnalysisChatMessage | null>(null);
  const [fixOpen, setFixOpen] = useState(false);
  const [sourceSelections, setSourceSelections] = useState<Record<string, ChatFixSourceSelection>>({});
  const createRequestIDRef = useRef(newAnalysisChatRequestID());
  const restoreControllerRef = useRef<AbortController | null>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const cancelControllerRef = useRef<AbortController | null>(null);
  const correctionControllerRef = useRef<AbortController | null>(null);
  const identityRef = useRef("");
  const messageListRef = useRef<HTMLDivElement | null>(null);
  const analysisRefRef = useRef(analysisRef);
  const patternScope = analysisRef.scope === "pattern";
  const history = useMemo(() => session ? analysisChatHistory(session) : [], [session]);
  const recordProgress = useCallback((progress: AnalysisChatProgress) => {
    setProgressPhase(progress.phase);
    if (progress.started_at) setProgressStartedAt(progress.started_at);
    setValidationRetries(progress.validation_retries ?? 0);
    setMaxValidationRetries(progress.max_validation_retries ?? 0);
    const usage = analysisChatProgressTurnUsage(progress);
    if (!usage) return;
    setTurnLimitRejected(usage.used >= usage.max);
    setSession((current) => current ? { ...current, turns_used: usage.used, max_turns: usage.max } : current);
  }, []);

  const identity = useMemo(
    () =>
      [
        analysisRef.job_id,
        analysisRef.scope,
        analysisRef.build_id,
        analysisRef.test_name,
        analysisRef.source,
        analysisRef.suite_name,
        analysisRef.class_name,
        analysisRef.junit_file,
        analysisRef.analysis_generated_at,
        analysisRef.pattern_id,
        analysisRef.pattern_hash,
      ].join("\u0000"),
    [analysisRef],
  );
  analysisRefRef.current = analysisRef;
  identityRef.current = identity;

  useEffect(() => {
    restoreControllerRef.current?.abort();
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
    correctionControllerRef.current?.abort();
    setExpanded(false);
    setQuestion("");
    setSession(null);
    setRestoring(false);
    setBusy(false);
    setError(null);
    setTurnLimitRejected(false);
    setPendingTurn(null);
    setContinueMode(false);
    setProgressPhase("queued");
    setProgressStartedAt(undefined);
    setValidationRetries(0);
    setMaxValidationRetries(0);
    setCancelling(false);
    setCorrectionPreview(null);
    setCorrectionOpen(false);
    setCorrectionBusy(false);
    setCorrectionError(null);
    setFixMessage(null);
    setFixOpen(false);
    setSourceSelections({});
    createRequestIDRef.current = newAnalysisChatRequestID();
  }, [identity]);

  useEffect(() => {
    if (!features.analysis_chat || auth.status !== "authenticated") {
      restoreControllerRef.current?.abort();
      if (auth.status !== "loading") {
        setSession(null);
        setRestoring(false);
      }
      return;
    }
    const restoreIdentity = identity;
    const controller = new AbortController();
    restoreControllerRef.current?.abort();
    restoreControllerRef.current = controller;
    setRestoring(true);
    setError(null);
    void (async () => {
      let restoredTurn: PendingTurn | null = null;
      try {
        const restored = await findAnalysisChatSession(analysisRefRef.current, controller.signal);
        if (identityRef.current !== restoreIdentity) return;
        setSession(restored);
        setRestoring(false);
        if (!restored?.active) return;

        restoredTurn = {
          sessionID: restored.id,
          requestID: restored.active.request_id,
          question: restored.active.question ?? "",
        };
        setPendingTurn(restoredTurn);
        setQuestion(restoredTurn.question);
        setProgressPhase(restored.active.phase);
        setBusy(true);
        const updated = await resumeAnalysisChatTurn(
          restored,
          recordProgress,
          controller.signal,
        );
        if (identityRef.current !== restoreIdentity) return;
        setSession(updated);
        const restoredState = restoredTurn ? analysisChatRequestState(updated, restoredTurn.requestID) : "unresolved";
        if (restoredState === "answered" || restoredState === "succeeded") {
          setQuestion("");
          setPendingTurn(null);
          setContinueMode(false);
          setError(null);
        } else if (restoredState === "terminal") {
          setPendingTurn(null);
          setError(null);
        } else {
          setPendingTurn(null);
          setQuestion(restoredTurn?.question ?? "");
          setContinueMode(true);
          setError("The restored question ended without an answer. Select Continue to try again.");
        }
      } catch (restoreError) {
        if (restoreError instanceof Error && restoreError.name === "AbortError") return;
        if (identityRef.current !== restoreIdentity) return;
        let reconciled: AnalysisChatSession | null = null;
        if (restoredTurn) {
          try {
            reconciled = await getAnalysisChatSession(restoredTurn.sessionID, controller.signal);
            if (identityRef.current !== restoreIdentity) return;
            setSession(reconciled);
          } catch (reconcileError) {
            if (reconcileError instanceof Error && reconcileError.name === "AbortError") return;
          }
        }
        const restoredRequestID = restoredTurn?.requestID;
        if (restoredRequestID && reconciled) {
          const restoredState = analysisChatRequestState(reconciled, restoredRequestID);
          if (restoredState === "answered" || restoredState === "succeeded") {
            setQuestion("");
            setPendingTurn(null);
            setError(null);
            return;
          }
          if (restoredState === "terminal") {
            setPendingTurn(null);
            setError(null);
            return;
          }
        }
        if (restoredTurn && isAmbiguousAnalysisChatFailure(restoreError)) {
          setPendingTurn(restoredTurn);
          setContinueMode(true);
          setError("The restored question may still be running. Select Continue to reconnect.");
        } else {
          setPendingTurn(null);
          setError(readableError(restoreError));
        }
      } finally {
        if (restoreControllerRef.current === controller) {
          restoreControllerRef.current = null;
          if (identityRef.current === restoreIdentity) {
            setRestoring(false);
            setBusy(false);
          }
        }
      }
    })();
    return () => controller.abort();
  }, [auth.status, features.analysis_chat, identity, recordProgress]);

  useEffect(() => {
    if (!expanded || (history.length === 0 && !busy)) return;
    const list = messageListRef.current;
    if (!list) return;
    list.scrollTo({ top: list.scrollHeight, behavior: "smooth" });
  }, [busy, expanded, history.length]);

  useEffect(() => () => {
    restoreControllerRef.current?.abort();
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
    correctionControllerRef.current?.abort();
  }, []);

  if (!features.analysis_chat) return null;

  const turnUsage = session ? analysisChatTurnUsage(session) : null;
  const turnLimitReached = analysisChatTurnLimitReached(session, pendingTurn !== null, turnLimitRejected);
  const questions = patternScope ? patternSuggestedQuestions : suggestedQuestions;

  async function submit(nextQuestion?: string) {
    const value = (nextQuestion ?? pendingTurn?.question ?? question).trim();
    if (!value || busy || restoring || turnLimitReached) return;
    if (pendingTurn && pendingTurn.question !== value) {
      setError("The previous question may still be running. Select Continue before asking another question.");
      return;
    }
    if (auth.status === "anonymous") {
      auth.signIn();
      return;
    }
    if (auth.status !== "authenticated") return;

    setContinueMode(false);
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy(true);
    setError(null);
    setProgressPhase("queued");
    setProgressStartedAt(undefined);
    setValidationRetries(0);
    setMaxValidationRetries(0);
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
      const updated = await streamAnalysisChatMessage(
        activeTurn.sessionID,
        activeTurn.question,
        activeTurn.requestID,
        recordProgress,
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

      let reconciled: AnalysisChatSession | null = null;
      if (activeSession) {
        try {
          reconciled = await getAnalysisChatSession(activeSession.id, controller.signal);
          setSession(reconciled);
          const activeRequestID = activeTurn?.requestID;
          if (activeRequestID) {
            const requestState = analysisChatRequestState(reconciled, activeRequestID);
            if (requestState === "answered" || requestState === "succeeded") {
              setQuestion("");
              setPendingTurn(null);
              return;
            }
            if (requestState === "terminal") {
              setPendingTurn(null);
              setError(null);
              return;
            }
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
      }

      const ambiguousFailure = isAmbiguousAnalysisChatFailure(requestError);
      if (activeSession && activeTurn && ambiguousFailure) {
        setPendingTurn(activeTurn);
        setContinueMode(true);
        setError("The question may still be running. Select Continue to reconnect to the same request.");
        return;
      }
      if (!activeSession && ambiguousFailure) {
        setContinueMode(true);
        setError("The conversation may have been created. Select Continue to reconnect to the same session.");
        return;
      }

      const exhausted =
        requestError instanceof AnalysisChatAPIError &&
        requestError.status === 429 &&
        requestError.message === analysisChatTurnLimitMessage;
      if (exhausted) {
        setTurnLimitRejected(true);
        setSession((current) => current ? markAnalysisChatTurnLimitReached(current) : current);
        setPendingTurn(null);
        setError(reconciled ? null : readableError(requestError));
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
    const requestIdentity = identity;
    correctionControllerRef.current?.abort();
    const controller = new AbortController();
    correctionControllerRef.current = controller;
    setCorrectionBusy(true);
    setCorrectionError(null);
    try {
      const preview = await previewAnalysisCorrection(session.id, requestID, controller.signal);
      if (identityRef.current !== requestIdentity || correctionControllerRef.current !== controller) return;
      setCorrectionPreview(preview);
      setCorrectionOpen(true);
    } catch (previewError) {
      if (previewError instanceof Error && previewError.name === "AbortError") return;
      if (identityRef.current === requestIdentity) setError(previewError instanceof Error ? previewError.message : "Could not prepare the correction.");
    } finally {
      if (correctionControllerRef.current === controller) {
        correctionControllerRef.current = null;
        if (identityRef.current === requestIdentity) setCorrectionBusy(false);
      }
    }
  }

  async function publishCorrection() {
    if (!correctionPreview) return;
    const requestIdentity = identity;
    correctionControllerRef.current?.abort();
    const controller = new AbortController();
    correctionControllerRef.current = controller;
    setCorrectionBusy(true);
    setCorrectionError(null);
    try {
      await confirmAnalysisCorrection(correctionPreview.token, controller.signal);
      if (identityRef.current !== requestIdentity) return;
      setCorrectionOpen(false);
      setCorrectionPreview(null);
      onCorrectionChanged?.();
    } catch (confirmError) {
      if (confirmError instanceof Error && confirmError.name === "AbortError") return;
      if (identityRef.current !== requestIdentity) return;
      if (!(confirmError instanceof AnalysisCorrectionAPIError)) {
        setCorrectionOpen(false);
        setCorrectionPreview(null);
        onCorrectionChanged?.();
      } else {
        setCorrectionError(confirmError.message);
      }
    } finally {
      if (correctionControllerRef.current === controller) {
        correctionControllerRef.current = null;
        if (identityRef.current === requestIdentity) setCorrectionBusy(false);
      }
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

  function sourceInvestigationChanged(
    chatRequestID: string,
    requestID: string | null,
    view: SourceInvestigationView | null,
  ) {
    setSourceSelections((current) => {
      if (!requestID || !view) {
        if (!(chatRequestID in current)) return current;
        const next = { ...current };
        delete next[chatRequestID];
        return next;
      }
      return { ...current, [chatRequestID]: { requestID, view } };
    });
  }

  function openFix(message: AnalysisChatMessage) {
    if (auth.status === "anonymous") {
      auth.signIn();
      return;
    }
    if (auth.status !== "authenticated") return;
    setFixMessage(message);
    setFixOpen(true);
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
              ref={messageListRef}
              spacing={1.25}
              aria-live="polite"
              sx={{
                p: { xs: 1.25, sm: 1.5 },
                maxHeight: { xs: "min(62vh, 560px)", sm: "min(70vh, 680px)" },
                minHeight: 0,
                overflowY: "auto",
                overscrollBehavior: "contain",
                scrollbarGutter: "stable",
                scrollbarWidth: "thin",
                scrollbarColor: (theme) => `${theme.palette.divider} transparent`,
                "& > *": { flexShrink: 0 },
                "&::-webkit-scrollbar": { width: 8 },
                "&::-webkit-scrollbar-thumb": {
                  borderRadius: 999,
                  border: "2px solid transparent",
                  backgroundClip: "padding-box",
                  bgcolor: "action.disabled",
                },
              }}
            >
              {restoring && (
                <Typography role="status" variant="body2" color="text.secondary" sx={{ py: 0.5 }}>
                  Restoring conversation...
                </Typography>
              )}
              {!restoring && history.length === 0 && !busy && !pendingTurn && !turnLimitReached && (
                <Box sx={{ py: 0.5 }}>
                  <Typography variant="body2" sx={{ fontWeight: 650 }}>
                    {patternScope ? "Interrogate the pattern across builds." : "Interrogate the conclusion, not just the summary."}
                  </Typography>
                  <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.35, mb: 1.25 }}>
                    {patternScope
                      ? "Ask which builds agree, where they differ, or whether the shared cause holds up."
                      : "Ask for evidence, test another cause, or challenge what the agent missed."}
                  </Typography>
                  <Stack direction="row" spacing={0.75} useFlexGap sx={{ flexWrap: "wrap" }}>
                    {questions.map((suggestion) => (
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

              {history.map((entry) => {
                if (entry.kind === "attempt") {
                  return <AttemptSummary key={entry.key} attempt={entry.attempt} />;
                }
                const message = entry.message;
                if (message.role === "user") {
                  return <UserMessage key={entry.key} content={message.content} />;
                }
                return (
                  <AssistantMessage
                    key={entry.key}
                    message={message}
                    fileCtx={fileCtx}
                    correctionEnabled={!patternScope && Boolean(features.analysis_corrections)}
                    sourceInvestigationEnabled={!patternScope && Boolean(features.source_investigation)}
                    chatFixEnabled={Boolean(features.chat_fix)}
                    fixEligible={Boolean(message.request_id && message.citations?.length && fixPatterns.length)}
                    sessionID={session?.id ?? ""}
                    sourceRepository={session?.source_repository}
                    onReviewCorrection={(requestID) => void reviewCorrection(requestID)}
                    onUseForFix={() => openFix(message)}
                    onSourceInvestigationChange={(requestID, view) =>
                      sourceInvestigationChanged(message.request_id ?? "", requestID, view)
                    }
                  />
                );
              })}

              {busy && pendingTurn && (
                <ThinkingState
                  phase={progressPhase}
                  cancelling={cancelling}
                  startedAt={progressStartedAt}
                  validationRetries={validationRetries}
                  maxValidationRetries={maxValidationRetries}
                  onCancel={() => void cancelTurn()}
                />
              )}
              {error && <Alert severity="error" variant="outlined">{error}</Alert>}
            </Stack>

            <Box sx={{ px: { xs: 1.25, sm: 1.5 }, pb: 1.5 }}>
              {turnLimitReached ? (
                <Alert severity="info" variant="outlined">
                  This conversation reached its attempt limit.
                </Alert>
              ) : (
                <Stack direction="row" spacing={0.75} sx={{ alignItems: "center" }}>
                  <TextField
                    fullWidth
                    multiline
                    minRows={1}
                    maxRows={5}
                    value={question}
                    onChange={(event) => {
                      setContinueMode(false);
                      setQuestion(limitAnalysisChatQuestion(event.target.value));
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
                        event.preventDefault();
                        void submit();
                      }
                    }}
                    disabled={restoring || busy || pendingTurn !== null}
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
                  <Tooltip title={pendingTurn || continueMode ? "Continue" : "Send question"}>
                    <span>
                      <IconButton
                        color="primary"
                        aria-label={pendingTurn || continueMode ? "Continue" : "Send question"}
                        onClick={() => void submit()}
                        disabled={restoring || busy || (pendingTurn?.question ?? question).trim() === ""}
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
              {turnUsage && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  sx={{ display: "block", mt: 0.75, textAlign: "right" }}
                >
                  {`${turnUsage.used}/${turnUsage.max} attempts`}
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
        error={correctionError}
        onClose={() => setCorrectionOpen(false)}
        onConfirm={() => void publishCorrection()}
      />
      <ChatFixDialog
        open={fixOpen}
        sessionID={session?.id ?? ""}
        message={fixMessage}
        patterns={fixPatterns}
        source={fixMessage?.request_id ? sourceSelections[fixMessage.request_id] ?? null : null}
        onClose={() => setFixOpen(false)}
      />
    </Box>
  );
}
