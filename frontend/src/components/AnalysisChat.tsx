import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
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
  RestartAltOutlined,
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
  clearAnalysisChatPendingIntent,
  createAnalysisChatSession,
  deleteAnalysisChatSession,
  findAnalysisChatSession,
  isAmbiguousAnalysisChatFailure,
  isAnalysisChatOAuthExpired,
  limitAnalysisChatQuestion,
  loadAnalysisChatPendingIntent,
  markAnalysisChatTurnLimitReached,
  newAnalysisChatRequestID,
  reconcileAnalysisChatTurn,
  resumeAnalysisChatTurn,
  saveAnalysisChatPendingIntent,
  streamAnalysisChatMessage,
} from "../lib/analysisChat";
import { fileToUrl, type FileToUrlContext } from "../lib/utils";
import { AnalysisCorrectionAPIError, confirmAnalysisCorrection, previewAnalysisCorrection } from "../lib/analysisCorrections";
import { soft, softChipSx } from "../theme";
import { overviewLayout, overviewTypography, sectionBandSx } from "../theme/overview";
import type {
  AnalysisChatAssessment,
  AnalysisChatAttempt,
  AnalysisChatCitation,
  AnalysisChatMessage,
  AnalysisChatProgress,
  AnalysisChatProgressPhase,
  AnalysisChatReference,
  AnalysisChatSession,
  AnalysisChatUnverifiedReason,
} from "../types/analysisChat";
import { RichText } from "./RichText";
import { AnalysisCorrectionDialog } from "./AnalysisCorrectionDialog";
import type { AnalysisCorrectionPreview } from "../types/corrections";
import type { PatternAnalysis } from "../types/dashboard";
import { ChatFixDialog } from "./ChatFixDialog";
import { chatFixGroundedRequestIDs, chatFixVerifiedSourcePaths } from "../lib/chatFixEligibility";

interface PendingTurn {
  sessionID: string;
  requestID: string;
  question: string;
  requestRecorded?: boolean;
}

function analysisChatIntentStorage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

// The first prompt names an artifact source, because an artifact-grounded turn
// is what produces verified citations and makes an answer fix-eligible.
const suggestedQuestions = [
  "What does the build log show at the failure?",
  "What evidence supports this conclusion?",
  "What would disprove this root cause?",
  "Could this failure be transient?",
] as const;

const patternSuggestedQuestions = [
  "How do the failures differ across the identified causes?",
  "Which builds support each cause?",
  "Are any builds unclassified or outliers?",
  "What evidence would change the grouping?",
] as const;

const causeSuggestedQuestions = [
  "What evidence supports this cause across its builds?",
  "How do the member builds differ?",
  "What concrete change follows from this cause?",
  "What evidence would disprove this cause?",
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

const unverifiedReasonDetail: Record<AnalysisChatUnverifiedReason, string> = {
  citation: "The quoted evidence did not match what the artifact tools returned.",
  reference: "The cited artifact was never read in this conversation.",
  missing: "The answer claimed artifact evidence but cited none.",
  format: "The answer did not follow the response format, so its evidence could not be verified.",
};

function isPendingAnalysisChatFailure(error: unknown): boolean {
  return error instanceof AnalysisChatAPIError &&
    error.status === 409 &&
    (error.message === analysisChatSessionBusyMessage || error.message === analysisChatRequestPendingMessage);
}

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
      case 422:
        return "The model reply could not be read. Try asking again.";
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
        // A squared block with the page's own surface and divider, rather than
        // an asymmetric chat bubble. The left accent is what marks it as the
        // reader's turn, so the shape does not have to.
        borderRadius: 1,
        bgcolor: "surface.containerHigh",
        border: "1px solid",
        borderColor: "divider",
        boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
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
          <Typography variant="caption" color="textSecondary" sx={{ ml: { xs: 2, sm: 5 } }}>
            Question text is unavailable for this earlier attempt.
          </Typography>
        )}
      <Alert severity={severity} variant="outlined" sx={{ py: 0.25 }}>
        <Typography variant="body2" sx={{ fontWeight: 700 }}>{status.label}</Typography>
        <Typography variant="caption" color="textSecondary">{status.detail}</Typography>
      </Alert>
    </Stack>
  );
}

function AssistantMessage({
  message,
  fileCtx,
  correctionEnabled,
  chatFixEnabled,
  fixEligible,
  fixIneligibleReason,
  onReviewCorrection,
  onUseForFix,
}: {
  message: AnalysisChatMessage;
  fileCtx: FileToUrlContext;
  correctionEnabled: boolean;
  chatFixEnabled: boolean;
  fixEligible: boolean;
  fixIneligibleReason?: string;
  onReviewCorrection: (requestID: string) => void;
  onUseForFix: () => void;
}) {
  const assessment = message.assessment
    ? assessmentConfig[message.assessment]
    : assessmentConfig.explains;
  const unverified = Boolean(message.unverified);
  const partiallyVerified = !unverified && Boolean(message.evidence_warnings?.length);
  const evidenceWarning = unverified || partiallyVerified;
  const accent = evidenceWarning ? "warning" : assessment.color === "default" ? "primary" : assessment.color;
  return (
    <Box
      sx={{
        border: evidenceWarning ? "2px dashed" : "1px solid",
        borderColor: (theme) => soft(theme, accent, evidenceWarning ? 0.55 : 0.24),
        borderRadius: 1,
        bgcolor: (theme) => soft(theme, accent, evidenceWarning ? 0.09 : 0.045),
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
          icon={evidenceWarning ? <ReportProblemOutlined /> : undefined}
          label={unverified ? "Unverified" : partiallyVerified ? "Partially verified" : assessment.label}
          sx={(theme) => ({
            ml: "auto",
            height: 22,
            fontSize: "0.68rem",
            // A verdict is a label, not a control, so it is tinted rather than
            // outlined. Evidence warnings keep a filled chip's weight.
            ...(evidenceWarning
              ? { bgcolor: "warning.main", color: "warning.contrastText", fontWeight: 750 }
              : { fontWeight: 600, ...softChipSx(theme, accent) }),
            "& .MuiChip-icon": { color: "inherit", fontSize: 15 },
          })}
        />
      </Stack>
      <Stack spacing={1.5} sx={{ p: 1.5 }}>
        {unverified && (
          <Box>
            <Typography variant="caption" sx={{ color: "warning.main", fontWeight: 650 }}>
              {message.unverified_reason ? unverifiedReasonDetail[message.unverified_reason] : ""}
              {" "}
              Treat this answer as unproven and read the artifacts before acting on it.
            </Typography>
            {message.evidence_warnings?.map((warning) => (
              <Typography key={warning} variant="caption" component="div" color="textSecondary">
                {warning}
              </Typography>
            ))}
          </Box>
        )}
        {partiallyVerified && (
          <Alert severity="warning" variant="outlined" sx={{ py: 0.25 }}>
            <Typography variant="caption" sx={{ display: "block", fontWeight: 650 }}>
              Some citations were omitted or could not be verified. The evidence shown below is verified.
            </Typography>
            {message.evidence_warnings?.map((warning) => (
              <Typography key={warning} variant="caption" component="div" color="textSecondary">
                {warning}
              </Typography>
            ))}
          </Alert>
        )}
        <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.65 }}>
          <RichText text={message.content} steps fileCtx={fileCtx} />
        </Typography>
        {(message.elapsed_ms || message.provider_ms || message.validation_retries) && (
          // Timings are data, so they read as the same monospace run the rest
          // of the dashboard uses, not as a row of bordered chips.
          <Typography component="div" color="textSecondary" sx={overviewTypography.data}>
            {[
              message.elapsed_ms ? `${(message.elapsed_ms / 1000).toFixed(1)}s total` : null,
              message.provider_ms ? `${(message.provider_ms / 1000).toFixed(1)}s provider` : null,
            ].filter(Boolean).join(" · ")}
            {message.validation_retries ? (
              <Box component="span" sx={{ color: "warning.main" }}>
                {(message.elapsed_ms || message.provider_ms) ? " · " : ""}
                {`${message.validation_retries} validation repair${message.validation_retries === 1 ? "" : "s"}`}
              </Box>
            ) : null}
          </Typography>
        )}

        {message.citations && message.citations.length > 0 && (
          <Box>
            <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", mb: 0.75 }}>
              <FactCheckOutlined sx={{ fontSize: 16, color: "success.main" }} />
              <Typography variant="label" color="textSecondary" sx={{ fontWeight: 700 }}>
                Verified evidence
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
                        <Typography component="span" variant="caption" color="textSecondary">
                          {lines}
                        </Typography>
                      )}
                    </Stack>
                    {citation.quote && (
                      <Typography
                        component="blockquote"
                        variant="caption"
                        color="textSecondary"
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
              borderRadius: 1,
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
              <Chip
                size="small"
                label="Not published"
                sx={(theme) => ({
                  ml: "auto",
                  height: 22,
                  fontSize: "0.68rem",
                  fontWeight: 600,
                  ...softChipSx(theme, "warning"),
                })}
              />
            </Stack>
            <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700 }}>
              Revised root cause
            </Typography>
            <Typography variant="body2" sx={{ mt: 0.25, lineHeight: 1.6 }}>
              <RichText text={message.proposed_revision.root_cause} steps fileCtx={fileCtx} />
            </Typography>
            <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700, mt: 1.25 }}>
              Revised remediation
            </Typography>
            <Typography variant="body2" sx={{ mt: 0.25, lineHeight: 1.6 }}>
              <RichText text={message.proposed_revision.suggested_fix} steps fileCtx={fileCtx} />
            </Typography>
            {correctionEnabled && !unverified && !partiallyVerified && message.request_id && (
              <Button
                size="small"
                variant="text"
                color="warning"
                startIcon={<PublishedWithChangesOutlined sx={{ fontSize: 17 }} />}
                onClick={() => onReviewCorrection(message.request_id!)}
                sx={{ mt: 1.5, ml: -0.5 }}
              >
                Review correction
              </Button>
            )}
          </Box>
        )}


        {chatFixEnabled && !unverified && fixEligible && message.request_id && (
          <Button
            size="small"
            variant="text"
            startIcon={<BuildOutlined sx={{ fontSize: 17 }} />}
            onClick={onUseForFix}
            sx={{ alignSelf: "flex-start", ml: -0.5 }}
          >
            Use this finding in a fix proposal
          </Button>
        )}
        {chatFixEnabled && !unverified && !fixEligible && fixIneligibleReason && message.request_id && (
          <Alert severity="info" variant="outlined" sx={{ py: 0.25 }}>
            {fixIneligibleReason}
          </Alert>
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
  validation_retrying: { title: "Validating evidence", detail: "The response or its evidence did not pass validation and is being retried." },
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
      alignItems: "center", borderRadius: 1, px: 1.5, py: 1.25,
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
        <Typography variant="caption" color="textSecondary" sx={{ display: "block" }}>{copy.detail}</Typography>
        <Typography variant="caption" color="textSecondary">
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
  appearance = "default",
}: {
  analysisRef: AnalysisChatReference;
  fileCtx: FileToUrlContext;
  fixPatterns?: PatternAnalysis[];
  onCorrectionChanged?: () => void;
  appearance?: "default" | "detail";
}) {
  const { features } = useCapabilities();
  const { status: authStatus, mode: authMode, signIn } = useAuth();
  const detailAppearance = appearance === "detail";
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
  const [resetOpen, setResetOpen] = useState(false);
  const [resetting, setResetting] = useState(false);
  const createRequestIDRef = useRef(newAnalysisChatRequestID());
  const restoreControllerRef = useRef<AbortController | null>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const cancelControllerRef = useRef<AbortController | null>(null);
  const correctionControllerRef = useRef<AbortController | null>(null);
  const resetControllerRef = useRef<AbortController | null>(null);
  const identityRef = useRef("");
  const messageListRef = useRef<HTMLDivElement | null>(null);
  const analysisRefRef = useRef(analysisRef);
  const patternScope = analysisRef.scope === "pattern";
  const causeScope = analysisRef.scope === "cause";
  const multiBuildScope = patternScope || causeScope;
  const chatTitle = causeScope ? "Investigate cause" : "Investigate and fix";
  const chatToggleLabel = `${expanded ? "Collapse" : "Expand"} ${chatTitle.toLowerCase()}`;
  const history = useMemo(() => session ? analysisChatHistory(session) : [], [session]);
  const groundedRequestIDs = useMemo(() => chatFixGroundedRequestIDs(session?.messages), [session]);
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
        analysisRef.causal_group_id,
        analysisRef.causal_group_hash,
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
    resetControllerRef.current?.abort();
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
    setResetOpen(false);
    setResetting(false);
    createRequestIDRef.current = newAnalysisChatRequestID();
  }, [identity]);

  useEffect(() => {
    if (!features.analysis_chat || authStatus !== "authenticated") {
      restoreControllerRef.current?.abort();
      if (authStatus !== "loading") {
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

        const restoredRecorded = loadAnalysisChatPendingIntent(
          analysisChatIntentStorage(),
          restoreIdentity,
          restored.id,
          restored.active.request_id,
        );
        restoredTurn = {
          sessionID: restored.id,
          requestID: restored.active.request_id,
          question: restored.active.question ?? "",
          requestRecorded: restoredRecorded,
        };
        setPendingTurn(restoredTurn);
        setQuestion(restoredTurn.question);
        setProgressPhase(restored.active.phase);
        setBusy(true);
        const updated = await resumeAnalysisChatTurn(
          restored,
          recordProgress,
          { requestRecorded: restoredRecorded, signal: controller.signal },
        );
        if (identityRef.current !== restoreIdentity) return;
        setSession(updated);
        const restoredState = restoredTurn ? analysisChatRequestState(updated, restoredTurn.requestID) : "unresolved";
        if (restoredState === "answered" || restoredState === "succeeded") {
          setQuestion("");
          if (restoredTurn) clearAnalysisChatPendingIntent(analysisChatIntentStorage(), restoredTurn.sessionID, restoredTurn.requestID);
          setPendingTurn(null);
          setContinueMode(false);
          setError(null);
        } else if (restoredState === "terminal") {
          if (restoredTurn) clearAnalysisChatPendingIntent(analysisChatIntentStorage(), restoredTurn.sessionID, restoredTurn.requestID);
          setPendingTurn(null);
          setError(null);
        } else if (restoredTurn?.requestRecorded === undefined) {
          setPendingTurn(null);
          setQuestion("");
          setContinueMode(false);
          setError("The restored request ended without an answer and its intent cannot be recovered safely. Select New conversation to start over.");
        } else {
          setPendingTurn(null);
          setQuestion(restoredTurn.question);
          setContinueMode(true);
          setError("The restored question ended without an answer. Select Continue to try again with the same intent.");
        }
      } catch (restoreError) {
        if (restoreError instanceof Error && restoreError.name === "AbortError") return;
        if (identityRef.current !== restoreIdentity) return;
        let reconciled: AnalysisChatSession | null = null;
        let reconciledState = "unresolved" as ReturnType<typeof analysisChatRequestState>;
        let reconcileError: unknown;
        if (restoredTurn) {
          clearAnalysisChatPendingIntent(analysisChatIntentStorage(), restoredTurn.sessionID, restoredTurn.requestID);
          restoredTurn = { ...restoredTurn, requestRecorded: undefined };
          try {
            const result = await reconcileAnalysisChatTurn(
              restoredTurn.sessionID,
              restoredTurn.requestID,
              recordProgress,
              { signal: controller.signal },
            );
            if (identityRef.current !== restoreIdentity) return;
            reconciled = result.session;
            reconciledState = result.state;
            setSession(reconciled);
          } catch (error) {
            if (error instanceof Error && error.name === "AbortError") return;
            reconcileError = error;
          }
        }
        const effectiveError = reconcileError ?? restoreError;
        if (isAnalysisChatOAuthExpired(effectiveError, authMode)) {
          signIn();
          return;
        }
        if (restoredTurn && reconciled) {
          if (reconciledState === "answered" || reconciledState === "succeeded") {
            setQuestion("");
            setPendingTurn(null);
            setError(null);
            return;
          }
          if (reconciledState === "terminal") {
            setPendingTurn(null);
            setError(null);
            return;
          }
          if (reconciledState === "pending") {
            setPendingTurn(restoredTurn);
            setQuestion(restoredTurn.question);
            setContinueMode(true);
            setError("The restored question is still running. Select Continue to observe the same request.");
            return;
          }
        }
        if (restoredTurn && isPendingAnalysisChatFailure(effectiveError)) {
          setPendingTurn(restoredTurn);
          setQuestion(restoredTurn.question);
          setContinueMode(true);
          setError("The restored question is still running. Select Continue to observe the same request.");
        } else if (restoredTurn && isAmbiguousAnalysisChatFailure(effectiveError)) {
          setPendingTurn(restoredTurn);
          setContinueMode(true);
          setError("The restored question may still be running. Select Continue to observe the same request.");
        } else {
          setPendingTurn(null);
          setError(readableError(effectiveError));
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
  }, [authMode, authStatus, features.analysis_chat, identity, recordProgress, signIn]);

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
    resetControllerRef.current?.abort();
  }, []);

  if (!features.analysis_chat) return null;

  const turnUsage = session ? analysisChatTurnUsage(session) : null;
  const turnLimitReached = analysisChatTurnLimitReached(session, pendingTurn !== null, turnLimitRejected);
  const questions = causeScope ? causeSuggestedQuestions : patternScope ? patternSuggestedQuestions : suggestedQuestions;
  const exactJUnitAnalysis = !multiBuildScope && analysisRef.source !== "build" && Boolean(analysisRef.junit_file);
  const exactFixEnabled = Boolean(features.junit_chat_fix) && exactJUnitAnalysis;
  const hasVerifiedSourcePaths = chatFixVerifiedSourcePaths(fileCtx.fileLinks, session?.source_repository).length > 0;
  // No published file link means no verified source path can exist, which is
  // conclusive before a session resolves the source repository.
  const fixSourceUnavailable = exactFixEnabled &&
    (session ? !hasVerifiedSourcePaths : Object.keys(fileCtx.fileLinks ?? {}).length === 0);

  async function submit(nextQuestion?: string) {
    const value = (nextQuestion ?? pendingTurn?.question ?? question).trim();
    if (!value || busy || restoring || turnLimitReached) return;
    if (pendingTurn && pendingTurn.question !== value) {
      setError("The previous question may still be running. Select Continue before asking another question.");
      return;
    }
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    if (authStatus !== "authenticated") return;

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
          requestRecorded: true,
        };
        setPendingTurn(activeTurn);
        setQuestion(value);
        saveAnalysisChatPendingIntent(analysisChatIntentStorage(), {
          analysisIdentity: identity,
          sessionID: activeTurn.sessionID,
          requestID: activeTurn.requestID,
          requestRecorded: activeTurn.requestRecorded ?? true,
        });
      }
      const updated = activeTurn.requestRecorded === undefined
        ? await resumeAnalysisChatTurn(activeSession, recordProgress, { signal: controller.signal })
        : await streamAnalysisChatMessage(
          activeTurn.sessionID,
          activeTurn.question,
          activeTurn.requestID,
          recordProgress,
          { requestRecorded: activeTurn.requestRecorded, signal: controller.signal },
        );
      setSession(updated);
      if (activeTurn.requestRecorded === undefined) {
        const requestState = analysisChatRequestState(updated, activeTurn.requestID);
        clearAnalysisChatPendingIntent(analysisChatIntentStorage(), activeTurn.sessionID, activeTurn.requestID);
        setPendingTurn(null);
        if (requestState === "answered" || requestState === "succeeded" || requestState === "terminal") {
          setQuestion("");
          return;
        }
        setQuestion("");
        setError("The restored request ended without an answer and its intent cannot be recovered safely. Select New conversation to start over.");
        return;
      }
      setQuestion("");
      clearAnalysisChatPendingIntent(analysisChatIntentStorage(), activeTurn.sessionID, activeTurn.requestID);
      setPendingTurn(null);
    } catch (requestError) {
      if (requestError instanceof Error && requestError.name === "AbortError") return;
      const observedTurn = activeTurn ? { ...activeTurn, requestRecorded: undefined } : null;
      if (activeTurn) {
        clearAnalysisChatPendingIntent(analysisChatIntentStorage(), activeTurn.sessionID, activeTurn.requestID);
      }
      if (isAnalysisChatOAuthExpired(requestError, authMode)) {
        signIn();
        return;
      }

      let reconciled: AnalysisChatSession | null = null;
      let reconciledState = "unresolved" as ReturnType<typeof analysisChatRequestState>;
      let reconcileError: unknown;
      if (activeSession && activeTurn) {
        try {
          const result = await reconcileAnalysisChatTurn(
            activeSession.id,
            activeTurn.requestID,
            recordProgress,
            { signal: controller.signal },
          );
          reconciled = result.session;
          reconciledState = result.state;
          setSession(reconciled);
          if (reconciledState === "answered" || reconciledState === "succeeded") {
            setQuestion("");
            setPendingTurn(null);
            return;
          }
          if (reconciledState === "terminal") {
            setPendingTurn(null);
            setError(null);
            return;
          }
          if (reconciledState === "pending" && observedTurn) {
            setPendingTurn(observedTurn);
            setQuestion(observedTurn.question);
            setContinueMode(true);
            setError("The question is still running. Select Continue to observe the same request.");
            return;
          }
        } catch (error) {
          if (error instanceof Error && error.name === "AbortError") return;
          reconcileError = error;
          if (isAnalysisChatOAuthExpired(error, authMode)) {
            signIn();
            return;
          }
        }
      }

      const effectiveError = reconcileError ?? requestError;
      if (activeSession && observedTurn && isPendingAnalysisChatFailure(effectiveError)) {
        setPendingTurn(observedTurn);
        setQuestion(observedTurn.question);
        setContinueMode(true);
        setError("The question is still running. Select Continue to observe the same request.");
        return;
      }
      const ambiguousFailure = isAmbiguousAnalysisChatFailure(effectiveError);
      if (activeSession && observedTurn && ambiguousFailure) {
        setPendingTurn(observedTurn);
        setContinueMode(true);
        setError("The question may still be running. Select Continue to observe the same request.");
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
        if (activeTurn) clearAnalysisChatPendingIntent(analysisChatIntentStorage(), activeTurn.sessionID, activeTurn.requestID);
        setPendingTurn(null);
        setError(reconciled ? null : readableError(requestError));
      } else {
        if (activeTurn) clearAnalysisChatPendingIntent(analysisChatIntentStorage(), activeTurn.sessionID, activeTurn.requestID);
        setPendingTurn(null);
        setError(readableError(effectiveError));
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

  async function startNewConversation() {
    if (resetting || restoring || busy) return;
    const resetIdentity = identity;
    const discarded = session;
    resetControllerRef.current?.abort();
    // A cancel that resolves after the reset would resubmit against the
    // discarded session, and a correction preview would reopen its dialog over
    // the fresh conversation.
    cancelControllerRef.current?.abort();
    correctionControllerRef.current?.abort();
    const controller = new AbortController();
    resetControllerRef.current = controller;
    setResetting(true);
    try {
      if (discarded) {
        if (pendingTurn) {
          clearAnalysisChatPendingIntent(analysisChatIntentStorage(), pendingTurn.sessionID, pendingTurn.requestID);
        }
        await deleteAnalysisChatSession(discarded.id, controller.signal);
      }
      if (identityRef.current !== resetIdentity) return;
      // The next question creates a replacement session, so a fresh create key
      // is what keeps that create from being deduped against the discarded one.
      createRequestIDRef.current = newAnalysisChatRequestID();
      setSession(null);
      setQuestion("");
      setError(null);
      setPendingTurn(null);
      setContinueMode(false);
      setTurnLimitRejected(false);
      setProgressPhase("queued");
      setProgressStartedAt(undefined);
      setValidationRetries(0);
      setMaxValidationRetries(0);
      setCancelling(false);
      setCorrectionPreview(null);
      setCorrectionOpen(false);
      setCorrectionError(null);
      setFixMessage(null);
      setFixOpen(false);
      setResetOpen(false);
    } catch (resetError) {
      if (resetError instanceof Error && resetError.name === "AbortError") return;
      if (identityRef.current !== resetIdentity) return;
      if (isAnalysisChatOAuthExpired(resetError, authMode)) {
        signIn();
        return;
      }
      setResetOpen(false);
      setError(readableError(resetError));
    } finally {
      if (resetControllerRef.current === controller) {
        resetControllerRef.current = null;
        if (identityRef.current === resetIdentity) setResetting(false);
      }
    }
  }

  function openFix(message: AnalysisChatMessage) {
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    if (authStatus !== "authenticated") return;
    setFixMessage(message);
    setFixOpen(true);
  }

  function toggleChat() {
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    setExpanded((value) => !value);
  }

  return (
    <Box sx={{ mt: detailAppearance ? 0 : 0.5 }}>
      {!detailAppearance && <Divider sx={{ mb: 1.5 }} />}
      <Box
        sx={{
          borderRadius: 1,
          border: detailAppearance ? 0 : "1px solid",
          borderColor: (theme) => soft(theme, "primary", 0.3),
          bgcolor: detailAppearance ? "transparent" : (theme) => soft(theme, "primary", 0.025),
          overflow: "hidden",
        }}
      >
        <Stack
          direction="row"
          spacing={0.25}
          sx={{
            alignItems: "center",
            px: detailAppearance ? 1.5 : 1,
            py: 0.5,
            borderTop: detailAppearance ? "1px solid" : 0,
            borderBottom: expanded || detailAppearance ? "1px solid" : 0,
            // On a detail page the chat is a peer of Run history and Runtime
            // trend, so it wears the same section band as they do instead of a
            // transparent bar with a tinted icon tile.
            ...(detailAppearance && {
              minHeight: overviewLayout.categoryBandMinHeight,
              ...sectionBandSx(),
            }),
            // Keep the color last: a border shorthand emitted after it resets
            // that edge back to currentColor, which reads as a white rule.
            borderColor: "divider",
          }}
        >
          {/* The heading wraps the toggle rather than sitting inside it: a
              heading is not valid phrasing content within a button, and
              assistive technology exposes that nesting inconsistently. */}
          <Box
            component={detailAppearance ? "h3" : "div"}
            sx={{
              m: 0,
              minWidth: 0,
              flex: 1,
              display: "flex",
              ...(detailAppearance ? overviewTypography.categoryHeading : {}),
            }}
          >
            <ButtonBase
              disableRipple
              onClick={toggleChat}
              disabled={authStatus === "loading" || authStatus === "unavailable"}
              aria-expanded={expanded}
              aria-controls="analysis-chat-content"
              sx={{
                minWidth: 0,
                flex: 1,
                justifyContent: "flex-start",
                gap: 1,
                borderRadius: 1,
                minHeight: 44,
                px: 0.5,
                py: 0.75,
                textAlign: "left",
                font: "inherit",
                color: "inherit",
                "&.Mui-disabled": { opacity: 0.5 },
              }}
            >
              <Box
                sx={{
                  width: detailAppearance ? 20 : 30,
                  height: detailAppearance ? 20 : 30,
                  display: "grid",
                  placeItems: "center",
                  borderRadius: 1,
                  bgcolor: detailAppearance ? "transparent" : (theme) => soft(theme, "primary", 0.14),
                  color: "primary.main",
                  flexShrink: 0,
                }}
              >
                <PsychologyAltOutlined sx={{ fontSize: detailAppearance ? 18 : 19 }} />
              </Box>
              <Box
                component="span"
                sx={detailAppearance ? undefined : { fontSize: "0.875rem", fontWeight: 750 }}
              >
                {chatTitle}
              </Box>
            </ButtonBase>
          </Box>
          {detailAppearance && turnUsage && (
            <Typography
              component="span"
              color="textSecondary"
              sx={{ ...overviewTypography.data, flexShrink: 0, whiteSpace: "nowrap" }}
            >
              {`${turnUsage.used}/${turnUsage.max} attempts`}
            </Typography>
          )}
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
            aria-label={chatToggleLabel}
            aria-expanded={expanded}
            aria-controls="analysis-chat-content"
            onClick={toggleChat}
            disabled={authStatus === "loading" || authStatus === "unavailable"}
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
                <Typography role="status" variant="body2" color="textSecondary" sx={{ py: 0.5 }}>
                  Restoring conversation...
                </Typography>
              )}
              {fixSourceUnavailable && (
                <Alert severity="info" variant="outlined">
                  Fix preview is not possible for this analysis: it has no verified immutable source path pinned to
                  the failing build's repository and commit. Questions still work, but no answer here can start a fix preview.
                </Alert>
              )}
              {!restoring && history.length === 0 && !busy && !pendingTurn && !turnLimitReached && (
                <Box sx={{ py: 0.5 }}>
                  <Typography variant="body2" sx={{ fontWeight: 650 }}>
                    {causeScope
                      ? "Interrogate this cause across its builds."
                      : patternScope
                        ? "Interrogate the pattern across builds."
                        : "Interrogate the conclusion, not just the summary."}
                  </Typography>
                  <Typography variant="caption" color="textSecondary" sx={{ display: "block", mt: 0.35, mb: 1.25 }}>
                    {causeScope
                      ? "Ask what the member builds prove, where they differ, or what concrete change follows."
                      : patternScope
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
                const hasArtifactEvidence = message.request_id
                  ? groundedRequestIDs.has(message.request_id)
                  : Boolean(message.citations?.length);
                const exactFixEligible = exactFixEnabled && hasArtifactEvidence && hasVerifiedSourcePaths;
                const legacyFixEligible = patternScope && Boolean(features.chat_fix) && hasArtifactEvidence &&
                  Boolean(fixPatterns.length);
                let fixIneligibleReason: string | undefined;
                if (exactFixEnabled && hasVerifiedSourcePaths && !hasArtifactEvidence) {
                  fixIneligibleReason = "Fix preview is not possible yet: no answer in this conversation carries a validated artifact citation. " +
                    "Ask something that requires reading an artifact, for example what the build log or JUnit file shows at the failure.";
                }
                return (
                  <AssistantMessage
                    key={entry.key}
                    message={message}
                    fileCtx={fileCtx}
                    correctionEnabled={!multiBuildScope && Boolean(features.analysis_corrections)}
                    chatFixEnabled={exactFixEnabled || Boolean(features.chat_fix && patternScope)}
                    fixEligible={exactFixEligible || legacyFixEligible}
                    fixIneligibleReason={fixIneligibleReason}
                    onReviewCorrection={(requestID) => void reviewCorrection(requestID)}
                    onUseForFix={() => openFix(message)}
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
                  This conversation reached its attempt limit. Start a new conversation to keep asking.
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
                          borderRadius: 1,
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
                          borderRadius: 1,
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
                            borderRadius: 1,
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
                <Stack
                  direction="row"
                  spacing={1}
                  sx={{ alignItems: "center", justifyContent: "space-between", mt: 0.75 }}
                >
                  <Button
                    size="small"
                    variant="text"
                    color="inherit"
                    startIcon={<RestartAltOutlined />}
                    onClick={() => setResetOpen(true)}
                    disabled={restoring || busy || resetting}
                    sx={{ color: "text.secondary", fontSize: "0.75rem" }}
                  >
                    New conversation
                  </Button>
                  {turnUsage && !detailAppearance && (
                    <Typography variant="caption" color="textSecondary">
                      {`${turnUsage.used}/${turnUsage.max} attempts`}
                    </Typography>
                  )}
                </Stack>
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
        exactAnalysis={!multiBuildScope}
        onClose={() => setFixOpen(false)}
      />
      <Dialog open={resetOpen} onClose={resetting ? undefined : () => setResetOpen(false)} fullWidth maxWidth="xs">
        <DialogTitle sx={{ display: "flex", alignItems: "center", gap: 1 }}>
          <RestartAltOutlined color="warning" />
          Start a new conversation
        </DialogTitle>
        <DialogContent>
          <Typography variant="body2" color="textSecondary">
            This discards the current conversation and its attempts. The transcript cannot be recovered, and the
            published analysis is unchanged.
          </Typography>
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2.5 }}>
          <Button onClick={() => setResetOpen(false)} disabled={resetting}>Keep conversation</Button>
          <Button
            variant="contained"
            color="warning"
            onClick={() => void startNewConversation()}
            disabled={resetting || restoring || busy}
          >
            {resetting ? "Discarding" : "Discard and start new"}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
