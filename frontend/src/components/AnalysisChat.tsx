import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import CircularProgress from "@mui/material/CircularProgress";
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
  ForumOutlined,
  HelpOutlined,
  PsychologyAltOutlined,
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
  analysisChatMarker,
  analysisChatRateLimitMessage,
  analysisChatRequestOutcomeUnknownMessage,
  analysisChatRequestPendingMessage,
  analysisChatRequestState,
  analysisChatSessionBusyMessage,
  analysisChatSessionReferencedMessage,
  analysisChatTurnLimitReached,
  analysisChatProgressTurnUsage,
  analysisChatTurnLimitMessage,
  analysisChatTurnUsage,
  AnalysisChatAPIError,
  cancelAnalysisChatRequest,
  clearAnalysisChatPendingIntent,
  createAnalysisChatSession,
  createPreparedAnalysisChatSession,
  deleteAnalysisChatSession,
  findAnalysisChatSession,
  getAnalysisChatSession,
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
import { soft, accentLabelSx, softChipSx } from "../theme";
import { dialogGutter, dialogPaperSx, overviewLayout, overviewTypography, sectionBandSx, touchTargetSx } from "../theme/overview";
import { DialogHeader } from "./ActionDialog";
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
import type { PatternAnalysis } from "../types/dashboard";
import type { CausalGroupFixTarget } from "../lib/patternFixGuidance";
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

// Width of the strip `scrollbar-gutter: stable` reserves on the message log.
// Platform-dependent; this matches the thin scrollbar Chromium renders.
const chatScrollbarGutter = 11;

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
          return "Another operator is using this shared investigation. You can follow their progress here.";
        }
        if (error.message === analysisChatSessionReferencedMessage) {
          return "This shared investigation is retained because a Fix proposal depends on it.";
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

function UserMessage({ content, actor }: { content: string; actor?: string }) {
  return (
    <Box
      sx={{
        ml: { xs: 2, sm: 5 },
        // A squared block on the page's own surface, rather than an asymmetric
        // chat bubble. The indent and a 1px accent edge mark the reader's turn;
        // the 3px edge is the section band's signature and is not borrowed here.
        borderRadius: 1,
        bgcolor: "surface.containerHigh",
        border: "1px solid",
        borderColor: "divider",
        borderInlineStartColor: "var(--mui-palette-primary-main)",
        px: 1.5,
        py: 1.1,
      }}
    >
      {actor && (
        <Typography variant="caption" color="textSecondary" sx={{ display: "block", mb: 0.25, fontWeight: 700 }}>
          {actor}
        </Typography>
      )}
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
        ? <UserMessage content={attempt.question} actor={attempt.actor} />
        : (
          <Typography variant="caption" color="textSecondary" sx={{ ml: { xs: 2, sm: 5 } }}>
            Question text is unavailable for this earlier attempt.
          </Typography>
        )}
      {/* Warning here means a cancelled or unknown past attempt, which is
          history rather than an urgent event, so only errors interrupt. */}
      <Alert
        severity={severity}
        role={severity === "error" ? "alert" : "status"}
        variant="outlined"
        sx={{ py: 0.25 }}
      >
        <Typography variant="body2" sx={{ fontWeight: 700 }}>{status.label}</Typography>
        <Typography variant="caption" color="textSecondary">{status.detail}</Typography>
      </Alert>
    </Stack>
  );
}

function AssistantMessage({
  message,
  fileCtx,
  chatFixEnabled,
  fixEligible,
  fixIneligibleReason,
  onUseForFix,
}: {
  message: AnalysisChatMessage;
  fileCtx: FileToUrlContext;
  chatFixEnabled: boolean;
  fixEligible: boolean;
  fixIneligibleReason?: string;
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
          {message.prepared ? "Prepared finding" : "Analysis agent"}
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
        {message.prepared && (
          <Typography variant="caption" color="textSecondary">
            Generated during the scheduled analysis run. Review or challenge it before opening a Fix proposal.
          </Typography>
        )}
        {unverified && (
          <Box>
            <Typography
              variant="caption"
              sx={(theme) => ({ ...accentLabelSx(theme, "warning"), fontWeight: 650 })}
            >
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
              <Box component="span" sx={(theme) => accentLabelSx(theme, "warning")}>
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
          </Box>
        )}


        {chatFixEnabled && !unverified && fixEligible && message.request_id && (
          <Chip
            label="Use this finding in a fix proposal"
            onClick={onUseForFix}
            icon={<BuildOutlined />}
            variant="outlined"
            sx={{
              // The message column stretches its children, and a chip that
              // spans the full width stops reading as a chip.
              alignSelf: "flex-start",
              height: "auto",
              ...touchTargetSx,
              borderColor: "divider",
              "& .MuiChip-label": { whiteSpace: "normal", py: 0.55, fontSize: "0.72rem" },
              "& .MuiChip-icon": { fontSize: 15 },
            }}
          />
        )}
        {chatFixEnabled && !unverified && !fixEligible && fixIneligibleReason && message.request_id && (
          // role="note": an eligibility explanation is not urgent, and the
          // conversation log already announces it when the answer arrives.
          <Alert severity="info" variant="outlined" role="note" sx={{ py: 0.25 }}>
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
  phase, actor, cancelling, startedAt, validationRetries, maxValidationRetries, onCancel,
}: {
  phase: AnalysisChatProgressPhase;
  actor?: string;
  cancelling: boolean;
  startedAt?: string;
  validationRetries: number;
  maxValidationRetries: number;
  onCancel?: () => void;
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
    // No live role here. The message list is the conversation's one live region,
    // and announcing this block again would double every progress update.
    <Stack direction="row" spacing={1.25} sx={{
      alignItems: "center", borderRadius: 1, px: 1.5, py: 1.25,
      bgcolor: (theme) => soft(theme, "primary", 0.055),
    }}>
      <Stack direction="row" spacing={0.4} aria-hidden="true">
        {[0, 1, 2].map((i) => (
          <Box key={i} sx={{
            width: 5, height: 5, borderRadius: "50%", bgcolor: "primary.main",
            animation: "analysisChatPulse 1.2s ease-in-out infinite", animationDelay: `${i * 150}ms`,
            // The dots are aria-hidden and the status line beside them names
            // the state, so the bounce stops without losing meaning.
            "@media (prefers-reduced-motion: reduce)": { animation: "none" },
            "@keyframes analysisChatPulse": {
              "0%, 70%, 100%": { opacity: 0.25, transform: "translateY(0)" },
              "35%": { opacity: 1, transform: "translateY(-3px)" },
            },
          }} />
        ))}
      </Stack>
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 650 }}>{copy.title}</Typography>
        <Typography variant="caption" color="textSecondary" sx={{ display: "block" }}>
          {actor ? `${actor} is using this shared investigation. ${copy.detail}` : copy.detail}
        </Typography>
        {/* The counter reads once a second. Announcing it would interrupt the
            operator on every tick for the whole turn, so it stays visual. */}
        <Typography variant="caption" color="textSecondary" aria-hidden="true">
          {elapsed !== null ? `${elapsed}s elapsed` : "Elapsed time unavailable"}
          {phase === "validation_retrying" && maxValidationRetries > 0
            ? ` · Validation retry ${validationRetries} of ${maxValidationRetries}` : ""}
        </Typography>
      </Box>
      {onCancel && (
        <Button size="small" variant="outlined" color="inherit" startIcon={<StopCircleOutlined />}
          onClick={() => { if (!cancelling) onCancel(); }}
          aria-disabled={cancelling || undefined}
          sx={{ flexShrink: 0, ...(cancelling && { opacity: 0.6 }) }}>
          {cancelling ? "Cancelling" : "Cancel"}
        </Button>
      )}
    </Stack>
  );
}

export function AnalysisChat({
  analysisRef,
  fileCtx,
  fixPatterns = [],
  fixTarget,
  appearance = "default",
  preparedFinding = false,
  onPreparedResolved,
}: {
  analysisRef: AnalysisChatReference;
  fileCtx: FileToUrlContext;
  fixPatterns?: PatternAnalysis[];
  fixTarget?: CausalGroupFixTarget;
  appearance?: "default" | "detail";
  // A prepared finding is waiting for this cause. Resolved read-only by the
  // caller, because the only path that opens one creates a shared session.
  preparedFinding?: boolean;
  // The server reported whether a prepared finding really was waiting. The
  // caller owns the answer because this panel unmounts when its cause card
  // folds away, which would otherwise forget a definitive miss.
  onPreparedResolved?: (ready: boolean) => void;
}) {
  const { features } = useCapabilities();
  const { status: authStatus, mode: authMode, signIn } = useAuth();
  const detailAppearance = appearance === "detail";
  // Several chats render on one page, so the panel each toggle controls needs
  // an id unique to that instance.
  const chatContentId = useId();
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
  const [fixMessage, setFixMessage] = useState<AnalysisChatMessage | null>(null);
  const [fixOpen, setFixOpen] = useState(false);
  const [resetOpen, setResetOpen] = useState(false);
  const [resetting, setResetting] = useState(false);
  const createRequestIDRef = useRef(newAnalysisChatRequestID());
  const preparedLookupIdentityRef = useRef("");
  const createPreparedSessionRef = useRef<() => void>(() => {});
  const preparedRetryTimerRef = useRef<number | null>(null);
  const [preparedRetryNonce, setPreparedRetryNonce] = useState(0);
  const restoreControllerRef = useRef<AbortController | null>(null);
  const controllerRef = useRef<AbortController | null>(null);
  const cancelControllerRef = useRef<AbortController | null>(null);
  const resetControllerRef = useRef<AbortController | null>(null);
  const identityRef = useRef("");
  const messageListRef = useRef<HTMLDivElement | null>(null);
  const turnLimitRef = useRef<HTMLDivElement | null>(null);
  const composerInputRef = useRef<HTMLTextAreaElement | null>(null);
  // Whether focus currently sits inside the chat panel. Removing a node does
  // not fire focusout, so this survives an in-flight control unmounting and is
  // what distinguishes orphaned focus from focus the operator moved away.
  const panelHadFocus = useRef(false);
  const analysisRefRef = useRef(analysisRef);
  const sessionRef = useRef<AnalysisChatSession | null>(null);
  const sessionGenerationRef = useRef(0);
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
  sessionRef.current = session;

  useEffect(() => {
    preparedLookupIdentityRef.current = "";
    if (preparedRetryTimerRef.current !== null) window.clearTimeout(preparedRetryTimerRef.current);
    preparedRetryTimerRef.current = null;
  }, [identity]);

  useEffect(() => {
    sessionGenerationRef.current += 1;
    restoreControllerRef.current?.abort();
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
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
        setProgressPhase(restored.active.phase);
        setProgressStartedAt(restored.active.started_at);
        setValidationRetries(restored.active.validation_retries ?? 0);
        setMaxValidationRetries(restored.active.max_validation_retries ?? 0);
        if (restoredRecorded === undefined) return;
        restoredTurn = {
          sessionID: restored.id,
          requestID: restored.active.request_id,
          question: restored.active.question ?? "",
          requestRecorded: restoredRecorded,
        };
        setPendingTurn(restoredTurn);
        setQuestion(restoredTurn.question);
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
    if (!features.analysis_chat || authStatus !== "authenticated" || !expanded || busy || restoring || resetting) return;
    const controller = new AbortController();
    const generation = sessionGenerationRef.current;
    const refreshIdentity = identity;
    let refreshing = false;
    const refresh = async () => {
      if (refreshing) return;
      refreshing = true;
      try {
        let refreshed: AnalysisChatSession | null;
        const current = sessionRef.current;
        if (current) {
          try {
            refreshed = await getAnalysisChatSession(current.id, controller.signal);
          } catch (refreshError) {
            if (!(refreshError instanceof AnalysisChatAPIError && refreshError.status === 404)) throw refreshError;
            refreshed = await findAnalysisChatSession(analysisRefRef.current, controller.signal);
          }
        } else {
          refreshed = await findAnalysisChatSession(analysisRefRef.current, controller.signal);
        }
        if (controller.signal.aborted || generation !== sessionGenerationRef.current || identityRef.current !== refreshIdentity) return;
        setSession(refreshed);
        if (refreshed?.active) recordProgress(refreshed.active);
      } catch (refreshError) {
        if (refreshError instanceof Error && refreshError.name === "AbortError") return;
        if (isAnalysisChatOAuthExpired(refreshError, authMode)) signIn();
      } finally {
        refreshing = false;
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), 2000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
  }, [authMode, authStatus, busy, expanded, features.analysis_chat, identity, recordProgress, resetting, restoring, signIn]);

  useEffect(() => {
    if (!expanded || (history.length === 0 && !busy)) return;
    const list = messageListRef.current;
    if (!list) return;
    // An explicit behavior overrides the CSS scroll-behavior rule, so the
    // preference has to be read here for the jump to be honored.
    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    list.scrollTo({ top: list.scrollHeight, behavior: reducedMotion ? "auto" : "smooth" });
  }, [busy, expanded, history.length]);

  useEffect(() => () => {
    restoreControllerRef.current?.abort();
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
    resetControllerRef.current?.abort();
    if (preparedRetryTimerRef.current !== null) window.clearTimeout(preparedRetryTimerRef.current);
  }, []);

  useEffect(() => {
    if (features.analysis_chat && causeScope && expanded && authStatus === "authenticated" && !session && !restoring && preparedLookupIdentityRef.current !== identity) {
      createPreparedSessionRef.current();
    }
  }, [authStatus, causeScope, expanded, features.analysis_chat, identity, preparedRetryNonce, restoring, session]);

  const turnLimitReached = analysisChatTurnLimitReached(session, pendingTurn !== null, turnLimitRejected);

  // Cancel, Continue and the composer itself come and go with the turn state,
  // and a removed node leaves focus on the body. When focus was ours and ended
  // up orphaned, put it on whatever still stands.
  useEffect(() => {
    if (!expanded || !panelHadFocus.current) return;
    if (document.activeElement !== document.body) return;
    panelHadFocus.current = false;
    (turnLimitReached ? turnLimitRef.current : composerInputRef.current)?.focus();
  }, [busy, expanded, pendingTurn, turnLimitReached]);

  if (!features.analysis_chat) return null;

  const turnUsage = session ? analysisChatTurnUsage(session) : null;
  const marker = analysisChatMarker({
    authenticated: authStatus === "authenticated",
    expanded,
    session,
    preparedFinding,
    restoring,
  });
  // A turn is in flight or the session is loading, so the composer accepts no
  // input but keeps its focus. Nothing here is ever natively disabled: that
  // drops the operator's focus to the document body mid-interaction.
  const composerLocked = busy || restoring || pendingTurn !== null || Boolean(session?.active);
  // Not composerLocked: a pending turn locks the field but leaves the button
  // live, because that button is what resumes it.
  const sendBlocked = busy || restoring || Boolean(session?.active) || (pendingTurn?.question ?? question).trim() === "";
  const questions = causeScope ? causeSuggestedQuestions : patternScope ? patternSuggestedQuestions : suggestedQuestions;
  const exactJUnitAnalysis = !multiBuildScope && analysisRef.source !== "build" && Boolean(analysisRef.junit_file);
  const causeFixEnabled = causeScope && Boolean(fixTarget);
  const exactFixEnabled = Boolean(features.junit_chat_fix) && (exactJUnitAnalysis || causeFixEnabled);
  const hasVerifiedSourcePaths = chatFixVerifiedSourcePaths(fileCtx.fileLinks, session?.source_repository).length > 0;
  // No published file link means no verified source path can exist, which is
  // conclusive before a session resolves the source repository.
  const fixSourceUnavailable = Boolean(features.junit_chat_fix) && exactJUnitAnalysis &&
    (session ? !hasVerifiedSourcePaths : Object.keys(fileCtx.fileLinks ?? {}).length === 0);

  async function submit(nextQuestion?: string) {
    const value = (nextQuestion ?? pendingTurn?.question ?? question).trim();
    if (!value || busy || restoring || turnLimitReached || session?.active) return;
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
      if (!activeTurn && activeSession.active) {
        recordProgress(activeSession.active);
        setError("Another operator is using this shared investigation. You can follow their progress here.");
        return;
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
      if (requestError instanceof AnalysisChatAPIError && requestError.message === analysisChatSessionBusyMessage) {
        setPendingTurn(null);
        setQuestion("");
        setContinueMode(false);
        if (activeSession) {
          try {
            setSession(await getAnalysisChatSession(activeSession.id, controller.signal));
          } catch {
            // The observer refresh will reconcile the shared session.
          }
        }
        setError("Another operator is using this shared investigation. You can follow their progress here.");
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
    sessionGenerationRef.current += 1;
    resetControllerRef.current?.abort();
    // A cancel that resolves after the reset would resubmit against the
    // discarded session.
    cancelControllerRef.current?.abort();
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


  function schedulePreparedRetry() {
    if (preparedRetryTimerRef.current !== null) window.clearTimeout(preparedRetryTimerRef.current);
    preparedRetryTimerRef.current = window.setTimeout(() => {
      preparedRetryTimerRef.current = null;
      preparedLookupIdentityRef.current = "";
      setPreparedRetryNonce((value) => value + 1);
    }, 30_000);
  }

  async function createPreparedSession() {
    if (session || restoring || authStatus !== "authenticated" || preparedLookupIdentityRef.current === identity) return;
    preparedLookupIdentityRef.current = identity;
    const controller = new AbortController();
    restoreControllerRef.current?.abort();
    restoreControllerRef.current = controller;
    setRestoring(true);
    setError(null);
    try {
      const created = await createPreparedAnalysisChatSession(analysisRef, createRequestIDRef.current, controller.signal);
      if (restoreControllerRef.current === controller) {
        onPreparedResolved?.(Boolean(created));
        if (created) setSession(created);
        else schedulePreparedRetry();
      }
    } catch (createError) {
      if (createError instanceof Error && createError.name === "AbortError") {
        preparedLookupIdentityRef.current = "";
        return;
      }
      if (restoreControllerRef.current === controller) {
        setError(readableError(createError));
        schedulePreparedRetry();
      }
    } finally {
      if (restoreControllerRef.current === controller) {
        restoreControllerRef.current = null;
        setRestoring(false);
      }
    }
  }

  createPreparedSessionRef.current = () => { void createPreparedSession(); };

  function toggleChat() {
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    if (expanded) {
      setExpanded(false);
      preparedLookupIdentityRef.current = "";
      if (preparedRetryTimerRef.current !== null) window.clearTimeout(preparedRetryTimerRef.current);
      preparedRetryTimerRef.current = null;
      return;
    }
    setExpanded(true);
    if (causeScope && !session && !restoring) void createPreparedSession();
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
              aria-controls={chatContentId}
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
          {marker && (
            <Tooltip title={marker.detail}>
              <Chip
                size="small"
                icon={marker.kind === "investigated" ? <ForumOutlined /> : <AutoAwesome />}
                label={marker.label}
                // The chip renders a plain element, so it needs a role for the
                // detail to reach assistive technology the tooltip cannot.
                role="note"
                aria-label={`${marker.label}. ${marker.detail}`}
                sx={(theme) => ({
                  flexShrink: 0,
                  maxWidth: 210,
                  height: 24,
                  fontWeight: 650,
                  ...overviewTypography.description,
                  ...softChipSx(theme, marker.kind === "investigated" ? "info" : "success"),
                  "& .MuiChip-icon": { color: "inherit", fontSize: 15 },
                })}
              />
            </Tooltip>
          )}
          {detailAppearance && turnUsage && (
            <Typography
              component="span"
              color="textSecondary"
              sx={{ ...overviewTypography.data, flexShrink: 0, whiteSpace: "nowrap" }}
            >
              {`${turnUsage.used}/${turnUsage.max} attempts`}
            </Typography>
          )}
          <Tooltip title="This shared conversation is visible to authenticated operators and does not change the published analysis">
            <IconButton
              disableRipple
              size="small"
              aria-label="This shared conversation is visible to authenticated operators and does not change the published analysis"
              sx={{ ...touchTargetSx, p: 0.5 }}
            >
              <HelpOutlined sx={{ color: "text.secondary", fontSize: 17 }} />
            </IconButton>
          </Tooltip>
          <IconButton
            disableRipple
            size="small"
            sx={touchTargetSx}
            aria-label={chatToggleLabel}
            aria-expanded={expanded}
            aria-controls={chatContentId}
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
          <Box
            id={chatContentId}
            onFocus={() => { panelHadFocus.current = true; }}
            onBlur={(event) => {
              // Moving between controls inside the panel is not leaving it.
              if (!event.currentTarget.contains(event.relatedTarget)) {
                panelHadFocus.current = false;
              }
            }}
          >
            <Stack
              ref={messageListRef}
              spacing={1.25}
              role="log"
              aria-live="polite"
              aria-label="Analysis conversation"
              sx={{
                p: { xs: 1.25, sm: 1.5 },
                // scrollbarGutter reserves a strip on the right that the
                // composer below does not lose, so the two would otherwise end
                // on different edges. Spend the right padding on that strip
                // instead and the message column matches the composer.
                pr: {
                  xs: `max(0px, calc(10px - ${chatScrollbarGutter}px))`,
                  sm: `max(0px, calc(12px - ${chatScrollbarGutter}px))`,
                },
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
                <Typography variant="body2" color="textSecondary" sx={{ py: 0.5 }}>
                  Restoring conversation...
                </Typography>
              )}
              {fixSourceUnavailable && (
                <Alert severity="info" variant="outlined" role="note">
                  Fix preview is not possible for this analysis: it has no verified immutable source path pinned to
                  the failing build's repository and commit. Questions still work, but no answer here can start a fix preview.
                </Alert>
              )}
              {!restoring && history.length === 0 && !busy && !pendingTurn && !session?.active && !turnLimitReached && (
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
                          ...touchTargetSx,
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
                  return <UserMessage key={entry.key} content={message.content} actor={message.actor} />;
                }
                const hasArtifactEvidence = message.request_id
                  ? groundedRequestIDs.has(message.request_id)
                  : Boolean(message.citations?.length);
                const exactFixEligible = exactFixEnabled && hasArtifactEvidence &&
                  (causeFixEnabled || hasVerifiedSourcePaths);
                const legacyFixEligible = patternScope && Boolean(features.chat_fix) && hasArtifactEvidence &&
                  Boolean(fixPatterns.length);
                let fixIneligibleReason: string | undefined;
                if (exactFixEnabled && (causeFixEnabled || hasVerifiedSourcePaths) && !hasArtifactEvidence) {
                  fixIneligibleReason = "Fix preview is not possible yet: no answer in this conversation carries a validated artifact citation. " +
                    "Ask something that requires reading an artifact, for example what the build log or JUnit file shows at the failure.";
                }
                return (
                  <AssistantMessage
                    key={entry.key}
                    message={message}
                    fileCtx={fileCtx}
                    chatFixEnabled={!session?.active && (exactFixEnabled || Boolean(features.chat_fix && patternScope))}
                    fixEligible={exactFixEligible || legacyFixEligible}
                    fixIneligibleReason={fixIneligibleReason}
                    onUseForFix={() => openFix(message)}
                  />
                );
              })}
  
              {(session?.active || (busy && pendingTurn)) && (
                <ThinkingState
                  phase={progressPhase}
                  actor={session?.active?.actor}
                  cancelling={cancelling}
                  startedAt={progressStartedAt ?? session?.active?.started_at}
                  validationRetries={validationRetries}
                  maxValidationRetries={maxValidationRetries}
                  onCancel={busy && pendingTurn ? () => void cancelTurn() : undefined}
                />
              )}
              {error && <Alert severity="error" variant="outlined">{error}</Alert>}
            </Stack>
  
            {/* aria-busy marks the composer, not the log: on the log it would
                sit above the progress update and defer announcing it. */}
            <Box
              aria-busy={composerLocked || restoring}
              sx={{
                px: { xs: 1.25, sm: 1.5 },
                pb: 1.5,
                // Field and Send read as one control, so the border lives on
                // the row and the field inside it is bare.
                "& > .MuiStack-root:first-of-type": {
                  alignItems: "flex-end",
                  gap: 0.5,
                  px: 0.75,
                  py: 0.625,
                  border: "1px solid",
                  borderColor: "divider",
                  borderRadius: 1,
                  bgcolor: "background.default",
                  "&:focus-within": { borderColor: "primary.main" },
                },
                // Matching the field's minimum to the Send button centres a
                // single line against it; the row still bottom-aligns once the
                // field grows past one line.
                "& .MuiOutlinedInput-root": {
                  bgcolor: "transparent",
                  p: 0,
                  px: 1,
                  minHeight: 34,
                  display: "flex",
                  alignItems: "center",
                },
                "& .MuiOutlinedInput-notchedOutline": { border: 0 },
                // This sizes the composer's buttons; their own sx cannot, since
                // a descendant selector outranks it. Touch keeps the 44px
                // target, keyed to the pointer rather than a breakpoint.
                "& .MuiIconButton-root": {
                  width: 34,
                  height: 34,
                  borderRadius: 1,
                  "@media (any-pointer: coarse)": { width: 44, height: 44 },
                },
              }}
            >
              {turnLimitReached ? (
                // Replacing the composer unmounts whatever the operator had
                // focused, so this takes the focus and says why input is gone.
                <Alert
                  ref={turnLimitRef}
                  role="status"
                  tabIndex={-1}
                  severity="info"
                  variant="outlined"
                  sx={{ "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 2 } }}
                >
                  This conversation reached its attempt limit. Start a new conversation to keep asking.
                </Alert>
              ) : (
                <Stack direction="row" spacing={0.75} sx={{ alignItems: "center" }}>
                  <TextField
                    fullWidth
                    multiline
                    minRows={1}
                    maxRows={5}
                    inputRef={composerInputRef}
                    value={question}
                    onChange={(event) => {
                      setContinueMode(false);
                      setQuestion(limitAnalysisChatQuestion(event.target.value));
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
                        event.preventDefault();
                        if (!composerLocked) void submit();
                      }
                    }}
                    // Read-only rather than disabled. Disabling the focused
                    // field drops focus to the document body, which strands the
                    // operator and puts Cancel out of reach.
                    placeholder="Ask why, challenge the cause, or test another hypothesis..."
                    slotProps={{
                      input: {
                        readOnly: composerLocked,
                        sx: {
                          borderRadius: 1,
                          bgcolor: "background.paper",
                          fontSize: "0.875rem",
                          // 16px wherever touch is available: iOS force-zooms a
                          // focused input below it, at any viewport width.
                          "@media (any-pointer: coarse)": { fontSize: "16px" },
                          ...(composerLocked && { opacity: 0.6 }),
                        },
                      },
                      htmlInput: {
                        "aria-label": "Ask about this analysis",
                        "aria-disabled": composerLocked || undefined,
                      },
                    }}
                  />
                  <Tooltip title={pendingTurn || continueMode ? "Continue" : "Send question"}>
                    <span>
                      <IconButton
                        color="primary"
                        aria-label={pendingTurn || continueMode ? "Continue" : "Send question"}
                        onClick={() => {
                          if (!sendBlocked) void submit();
                        }}
                        // Never natively disabled, for the same focus reason.
                        aria-disabled={sendBlocked || undefined}
                        sx={{
                          bgcolor: "primary.main",
                          color: "primary.contrastText",
                          "&:hover": { bgcolor: "primary.dark" },
                          ...(sendBlocked && { bgcolor: "action.disabledBackground", opacity: 0.6 }),
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
                          onClick={() => { if (!cancelling) void cancelTurn(); }}
                          aria-disabled={cancelling || undefined}
                          sx={{
                            border: "1px solid",
                            borderColor: "divider",
                            color: "text.secondary",
                            ...(cancelling && { opacity: 0.6 }),
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
                    disabled={restoring || busy || resetting || Boolean(session.active)}
                    sx={{ ...touchTargetSx, color: "text.secondary", fontSize: "0.875rem", fontWeight: 600 }}
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
      <ChatFixDialog
        open={fixOpen}
        sessionID={session?.id ?? ""}
        message={fixMessage}
        patterns={fixPatterns}
        exactAnalysis={!patternScope}
        causeScope={causeScope}
        onClose={() => setFixOpen(false)}
      />
      <Dialog
        open={resetOpen}
        onClose={resetting ? undefined : () => setResetOpen(false)}
        fullWidth
        maxWidth="xs"
        slotProps={{ paper: { sx: dialogPaperSx } }}
      >
        {/* Primary band like every other action dialog, so only the confirm
            button carries the destructive colour. */}
        <DialogHeader
          icon={<RestartAltOutlined sx={{ fontSize: 18 }} />}
          accent="primary"
          title="Start a new conversation"
        />
        <DialogContent dividers sx={{ px: dialogGutter, py: 2 }}>
          <Typography variant="body2" color="textSecondary">
            This removes the shared conversation for every operator. The transcript cannot be recovered, and the published analysis is unchanged.
          </Typography>
        </DialogContent>
        <DialogActions sx={{ px: dialogGutter, py: 2 }}>
          <Button sx={touchTargetSx} color="inherit" onClick={() => setResetOpen(false)} disabled={resetting}>
            Keep conversation
          </Button>
          <Button
            sx={touchTargetSx}
            variant="contained"
            color="warning"
            disableElevation
            startIcon={resetting ? <CircularProgress size={16} color="inherit" /> : undefined}
            onClick={() => void startNewConversation()}
            disabled={resetting || restoring || busy || Boolean(session?.active)}
          >
            {resetting ? "Removing" : "Remove and start new"}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
