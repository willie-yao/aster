import { useEffect, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import {
  CodeOutlined,
  FactCheckOutlined,
  ManageSearchOutlined,
  OpenInNewOutlined,
  RefreshOutlined,
  StopCircleOutlined,
  VerifiedOutlined,
} from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { AnalysisChatAPIError, newAnalysisChatRequestID } from "../lib/analysisChat";
import {
  cancelSourceInvestigation,
  getSourceInvestigation,
  sourceInvestigationActiveLimitMessage,
  sourceInvestigationIdempotencyConflictMessage,
  sourceInvestigationLimitMessage,
  sourceInvestigationNotFoundMessage,
  sourceInvestigationOutcomeUnknownMessage,
  sourceInvestigationPendingMessage,
  streamSourceInvestigation,
} from "../lib/sourceInvestigation";
import { fileToUrl, type FileToUrlContext } from "../lib/utils";
import { soft } from "../theme";
import type {
  SourceInvestigationCitation,
  SourceInvestigationConfidence,
  SourceInvestigationPhase,
  SourceInvestigationRelationship,
  SourceInvestigationView,
} from "../types/sourceInvestigation";
import { RichText } from "./RichText";

const phaseCopy: Record<SourceInvestigationPhase, { title: string; detail: string }> = {
  queued: { title: "Queued", detail: "Waiting for the source runtime." },
  cloning_source: { title: "Pinning the source", detail: "Checking out the exact revision from this Prow run." },
  investigating_source: { title: "Reading the implementation", detail: "Tracing the answer through the relevant source paths." },
  verifying_citations: { title: "Verifying citations", detail: "Matching every quote and line range against the pinned revision." },
  finalizing: { title: "Finalizing the finding", detail: "Preparing the verified source result." },
  cancelling: { title: "Cancelling", detail: "Stopping the read-only source task." },
};

const relationshipConfig: Record<
  SourceInvestigationRelationship,
  { label: string; color: "success" | "info" | "warning" | "default" }
> = {
  supports: { label: "Supports analysis", color: "success" },
  refines: { label: "Refines analysis", color: "info" },
  contradicts: { label: "Contradicts analysis", color: "warning" },
  inconclusive: { label: "Inconclusive", color: "default" },
};

const confidenceConfig: Record<
  SourceInvestigationConfidence,
  { label: string; color: "success" | "warning" | "default" }
> = {
  high: { label: "High confidence", color: "success" },
  medium: { label: "Medium confidence", color: "warning" },
  low: { label: "Low confidence", color: "default" },
};

function sourceErrorMessage(error: unknown): string {
  if (error instanceof AnalysisChatAPIError) {
    switch (error.status) {
      case 404:
        if (error.message === sourceInvestigationNotFoundMessage) {
          return "This source investigation is no longer available.";
        }
        return "This conversation is no longer available. Refresh the page to load the latest analysis.";
      case 409:
        if (error.message === sourceInvestigationPendingMessage) {
          return "The source investigation is still running. Reconnect to continue following it.";
        }
        if (error.message === sourceInvestigationOutcomeUnknownMessage) {
          return "The source task ended without a durable result after a server interruption. Run it again.";
        }
        if (error.message === sourceInvestigationIdempotencyConflictMessage) {
          return "The source request changed while reconnecting. Refresh the conversation before retrying.";
        }
        return error.message;
      case 429:
        if (error.message === sourceInvestigationActiveLimitMessage) {
          return "Another source investigation is already active for your account.";
        }
        if (error.message === sourceInvestigationLimitMessage) {
          return "This conversation reached its source investigation limit.";
        }
        return "The source investigation service is at capacity. Try again later.";
      case 499:
        return "The source investigation was cancelled.";
      case 504:
        return "The source investigation timed out before it could verify a result.";
      default:
        return error.message;
    }
  }
  if (error instanceof Error && error.name !== "AbortError") return error.message;
  return "The source investigation could not complete the request.";
}

function citationLines(citation: SourceInvestigationCitation): string {
  if (citation.line_start === citation.line_end) return `line ${citation.line_start}`;
  return `lines ${citation.line_start}-${citation.line_end}`;
}

function elapsedLabel(elapsedMs?: number): string | null {
  if (!elapsedMs || elapsedMs < 1) return null;
  if (elapsedMs < 1000) return `${elapsedMs}ms`;
  return `${(elapsedMs / 1000).toFixed(elapsedMs < 10000 ? 1 : 0)}s`;
}

function SourceProgress({
  phase,
  cancelling,
  onCancel,
}: {
  phase: SourceInvestigationPhase;
  cancelling: boolean;
  onCancel: () => void;
}) {
  const copy = phaseCopy[phase];
  return (
    <Box
      role="status"
      aria-live="polite"
      sx={{
        border: "1px solid",
        borderColor: (theme) => soft(theme, "primary", 0.3),
        bgcolor: (theme) => soft(theme, "primary", 0.045),
        borderRadius: "10px",
        overflow: "hidden",
      }}
    >
      <Stack direction="row" spacing={1.1} sx={{ alignItems: "center", px: 1.25, py: 1.1 }}>
        <Box
          sx={{
            width: 28,
            height: 28,
            display: "grid",
            placeItems: "center",
            borderRadius: "8px",
            color: "primary.main",
            bgcolor: (theme) => soft(theme, "primary", 0.12),
            flexShrink: 0,
          }}
        >
          <CodeOutlined sx={{ fontSize: 17 }} />
        </Box>
        <Box sx={{ minWidth: 0, flex: 1 }}>
          <Typography variant="body2" sx={{ fontWeight: 700 }}>
            {copy.title}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {copy.detail}
          </Typography>
        </Box>
        <Button
          size="small"
          color="inherit"
          variant="outlined"
          startIcon={<StopCircleOutlined />}
          onClick={onCancel}
          disabled={cancelling}
          sx={{ flexShrink: 0 }}
        >
          {cancelling ? "Cancelling" : "Cancel"}
        </Button>
      </Stack>
      <Box
        aria-hidden="true"
        sx={{
          height: 3,
          background: (theme) =>
            `linear-gradient(90deg, transparent 0%, ${theme.palette.primary.main} 48%, transparent 100%)`,
          animation: "sourceInvestigationScan 1.7s ease-in-out infinite",
          "@keyframes sourceInvestigationScan": {
            "0%": { transform: "translateX(-70%)", opacity: 0.2 },
            "50%": { opacity: 0.8 },
            "100%": { transform: "translateX(70%)", opacity: 0.2 },
          },
        }}
      />
    </Box>
  );
}

function SourceResult({ view, fileCtx }: { view: SourceInvestigationView; fileCtx: FileToUrlContext }) {
  const result = view.result;
  if (!result) return null;
  const relationship = relationshipConfig[result.relationship];
  const confidence = confidenceConfig[result.confidence];
  const elapsed = elapsedLabel(result.elapsed_ms);
  return (
    <Box
      sx={{
        border: "1px solid",
        borderColor: (theme) => soft(theme, relationship.color === "default" ? "primary" : relationship.color, 0.34),
        bgcolor: (theme) => soft(theme, relationship.color === "default" ? "primary" : relationship.color, 0.045),
        borderRadius: "11px",
        overflow: "hidden",
      }}
    >
      <Stack
        direction="row"
        spacing={0.75}
        useFlexGap
        sx={{ alignItems: "center", flexWrap: "wrap", px: 1.35, py: 1, borderBottom: "1px solid", borderColor: "divider" }}
      >
        <CodeOutlined sx={{ fontSize: 17, color: "primary.main" }} />
        <Typography variant="label" sx={{ fontWeight: 750 }}>
          Source investigation
        </Typography>
        <Chip size="small" icon={<VerifiedOutlined />} label="Verified" color="success" variant="outlined" sx={{ height: 23 }} />
        <Stack direction="row" spacing={0.5} sx={{ ml: { sm: "auto" } }}>
          <Chip size="small" label={relationship.label} color={relationship.color} variant="outlined" sx={{ height: 23 }} />
          <Chip size="small" label={confidence.label} color={confidence.color} variant="outlined" sx={{ height: 23 }} />
        </Stack>
      </Stack>
      <Stack spacing={1.4} sx={{ p: 1.35 }}>
        <Typography variant="body2" sx={{ lineHeight: 1.65 }}>
          <RichText text={result.finding} steps fileCtx={fileCtx} />
        </Typography>

        <Box
          sx={{
            borderLeft: "3px solid",
            borderColor: "primary.main",
            bgcolor: (theme) => soft(theme, "primary", 0.06),
            borderRadius: "0 8px 8px 0",
            px: 1.2,
            py: 0.85,
          }}
        >
          <Typography variant="caption" color="text.secondary" sx={{ display: "block", fontWeight: 750, mb: 0.2 }}>
            What to inspect next
          </Typography>
          <Typography variant="body2" sx={{ lineHeight: 1.55 }}>
            <RichText text={result.direction} steps fileCtx={fileCtx} />
          </Typography>
        </Box>

        {result.citations && result.citations.length > 0 && (
          <Box>
            <Stack direction="row" spacing={0.7} sx={{ alignItems: "center", mb: 0.8 }}>
              <FactCheckOutlined sx={{ fontSize: 16, color: "success.main" }} />
              <Typography variant="label" color="text.secondary" sx={{ fontWeight: 700 }}>
                Verified source evidence
              </Typography>
            </Stack>
            <Stack spacing={0.75}>
              {result.citations.map((citation, index) => {
                const url = fileToUrl(citation.path, fileCtx);
                return (
                  <Box
                    key={`${citation.path}-${citation.line_start}-${citation.line_end}-${index}`}
                    sx={{
                      border: "1px solid",
                      borderColor: "divider",
                      bgcolor: "background.paper",
                      borderRadius: "8px",
                      px: 1.1,
                      py: 0.85,
                    }}
                  >
                    <Stack direction="row" spacing={0.7} useFlexGap sx={{ alignItems: "baseline", flexWrap: "wrap" }}>
                      {url ? (
                        <Link
                          href={url}
                          target="_blank"
                          rel="noopener noreferrer"
                          sx={{ display: "inline-flex", alignItems: "center", gap: 0.4, fontFamily: "monospace", fontSize: "0.75rem", fontWeight: 700 }}
                        >
                          {citation.path}
                          <OpenInNewOutlined sx={{ fontSize: 12 }} />
                        </Link>
                      ) : (
                        <Typography component="span" sx={{ fontFamily: "monospace", fontSize: "0.75rem", fontWeight: 700 }}>
                          {citation.path}
                        </Typography>
                      )}
                      <Typography component="span" variant="caption" color="text.secondary">
                        {citationLines(citation)}
                      </Typography>
                      {citation.verified && (
                        <Typography component="span" variant="caption" color="success.main" sx={{ fontWeight: 700 }}>
                          verified
                        </Typography>
                      )}
                    </Stack>
                    <Typography
                      component="blockquote"
                      variant="caption"
                      color="text.secondary"
                      sx={{ m: 0, mt: 0.45, fontFamily: "monospace", lineHeight: 1.55, whiteSpace: "pre-wrap" }}
                    >
                      “{citation.quote}”
                    </Typography>
                  </Box>
                );
              })}
            </Stack>
          </Box>
        )}

        <Typography variant="caption" color="text.secondary">
          Read-only source review at the exact Prow revision{elapsed ? `, completed in ${elapsed}` : ""}.
        </Typography>
      </Stack>
    </Box>
  );
}

export function SourceInvestigationPanel({
  sessionID,
  chatRequestID,
  fileCtx,
}: {
  sessionID: string;
  chatRequestID: string;
  fileCtx: FileToUrlContext;
}) {
  const auth = useAuth();
  const [view, setView] = useState<SourceInvestigationView | null>(null);
  const [phase, setPhase] = useState<SourceInvestigationPhase>("queued");
  const [busy, setBusy] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const requestIDRef = useRef(newAnalysisChatRequestID());
  const controllerRef = useRef<AbortController | null>(null);
  const cancelControllerRef = useRef<AbortController | null>(null);
  const identity = `${sessionID}\u0000${chatRequestID}`;
  const identityRef = useRef(identity);
  identityRef.current = identity;

  useEffect(() => {
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
    requestIDRef.current = newAnalysisChatRequestID();
    setView(null);
    setPhase("queued");
    setBusy(false);
    setCancelling(false);
    setError(null);
  }, [identity]);

  useEffect(() => () => {
    controllerRef.current?.abort();
    cancelControllerRef.current?.abort();
  }, []);

  async function run(fresh = false, initialPhase?: SourceInvestigationPhase) {
    if (busy) return;
    if (auth.status === "anonymous") {
      auth.signIn();
      return;
    }
    if (auth.status !== "authenticated") return;
    if (fresh) requestIDRef.current = newAnalysisChatRequestID();

    const runIdentity = identity;
    const requestID = requestIDRef.current;
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setBusy(true);
    setCancelling(false);
    setError(null);
    setPhase(initialPhase ?? view?.phase ?? "queued");
    try {
      const updated = await streamSourceInvestigation(
        sessionID,
        chatRequestID,
        requestID,
        (progress) => {
          if (identityRef.current !== runIdentity || controllerRef.current !== controller) return;
          setPhase(progress.phase);
        },
        controller.signal,
      );
      if (identityRef.current !== runIdentity || controllerRef.current !== controller) return;
      setView(updated);
      setPhase(updated.phase ?? "finalizing");
    } catch (runError) {
      if (runError instanceof Error && runError.name === "AbortError") return;
      if (identityRef.current !== runIdentity || controllerRef.current !== controller) return;
      if (runError instanceof AnalysisChatAPIError && runError.status === 401 && auth.mode === "oauth") {
        auth.signIn();
        return;
      }
      try {
        const reconciled = await getSourceInvestigation(sessionID, requestID, controller.signal);
        if (identityRef.current !== runIdentity || controllerRef.current !== controller) return;
        setView(reconciled);
        setPhase(reconciled.phase ?? phase);
        if (reconciled.status === "succeeded") {
          setError(null);
        } else if (reconciled.status === "pending") {
          setError("The source investigation is still running. Reconnect to continue following it.");
        } else if (reconciled.status === "unknown") {
          setError("The source task ended without a durable result after a server interruption. Run it again.");
        } else {
          setError(sourceErrorMessage(runError));
        }
        return;
      } catch (reconcileError) {
        if (reconcileError instanceof Error && reconcileError.name === "AbortError") return;
      }
      setError(sourceErrorMessage(runError));
    } finally {
      if (controllerRef.current === controller) {
        controllerRef.current = null;
        if (identityRef.current === runIdentity) {
          setBusy(false);
          setCancelling(false);
        }
      }
    }
  }

  async function cancel() {
    if (cancelling) return;
    const cancelIdentity = identity;
    const requestID = requestIDRef.current;
    cancelControllerRef.current?.abort();
    const controller = new AbortController();
    cancelControllerRef.current = controller;
    setCancelling(true);
    setPhase("cancelling");
    try {
      await cancelSourceInvestigation(sessionID, requestID, controller.signal);
      if (!busy && identityRef.current === cancelIdentity) {
        await run(false, "cancelling");
      }
    } catch (cancelError) {
      if (cancelError instanceof Error && cancelError.name === "AbortError") return;
      if (identityRef.current === cancelIdentity) {
        setCancelling(false);
        setError(sourceErrorMessage(cancelError));
      }
    } finally {
      if (cancelControllerRef.current === controller) {
        cancelControllerRef.current = null;
        if (identityRef.current === cancelIdentity && !busy) setCancelling(false);
      }
    }
  }

  const pending = busy || view?.status === "pending";
  if (!view && !busy && !error) {
    return (
      <Stack direction={{ xs: "column", sm: "row" }} spacing={0.8} sx={{ alignItems: { sm: "center" }, pt: 0.15 }}>
        <Button
          size="small"
          variant="outlined"
          startIcon={<ManageSearchOutlined />}
          onClick={() => void run()}
          disabled={auth.status === "loading" || auth.status === "unavailable"}
          sx={{ alignSelf: { xs: "flex-start", sm: "center" } }}
        >
          Investigate source
        </Button>
        <Typography variant="caption" color="text.secondary">
          Verify this answer against the exact source revision.
        </Typography>
      </Stack>
    );
  }

  return (
    <Stack spacing={0.9}>
      {pending && <SourceProgress phase={phase} cancelling={cancelling} onCancel={() => void cancel()} />}
      {view?.status === "succeeded" && <SourceResult view={view} fileCtx={fileCtx} />}
      {error && (
        <Alert
          severity={view?.status === "unknown" ? "warning" : "error"}
          variant="outlined"
          action={
            <Button
              color="inherit"
              size="small"
              startIcon={<RefreshOutlined />}
              onClick={() => void run(view?.status === "failed" || view?.status === "unknown")}
              disabled={busy}
            >
              {view?.status === "pending" ? "Reconnect" : view?.status === "failed" || view?.status === "unknown" ? "Run again" : "Try again"}
            </Button>
          }
        >
          {error}
        </Alert>
      )}
    </Stack>
  );
}
