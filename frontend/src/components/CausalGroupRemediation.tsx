import { useEffect, useMemo, useRef, useState } from "react";
import { CloudOff, ExpandMore } from "@mui/icons-material";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import {
  getCausalRemediationStatus,
  previewCausalFix,
  startCausalRemediation,
  type CausalFixPreview,
  type CausalRemediationRef,
} from "../lib/causalRemediation";
import { causalRemediationBlockedReason, patternRemediationPresentation } from "../lib/patternRemediation";
import { overviewTypography } from "../theme/overview";
import type {
  PatternCausalGroup,
  PatternRemediationInvestigationSummary,
  PatternRemediationInvestigationState,
  PatternRemediationTargetSummary,
} from "../types/dashboard";

const activeStates = new Set<PatternRemediationInvestigationState>([
  "queued",
  "investigating",
  "verifying",
]);

// CausalGroupRemediation renders the remediation verdict for one causal group.
// Remediation is decided per cause, so each group owns its own state, polling,
// and controls.
export function CausalGroupRemediation({
  group,
  investigation,
  jobID,
  patternID,
  patternHash,
  patternEligible,
  chatAvailable,
}: {
  group: PatternCausalGroup;
  investigation?: PatternRemediationInvestigationSummary;
  jobID?: string;
  patternID?: string;
  patternHash?: string;
  // The resolver runs the investigation only on a recurring pattern, so a
  // cause inside an unclassified one must not be offered a control the server
  // would reject.
  patternEligible?: boolean;
  // Whether the pattern chat can actually run on this deployment, so a blocked
  // verdict never points at a path that is not there.
  chatAvailable?: boolean;
}) {
  const { features } = useCapabilities();
  const { status: authStatus, signIn } = useAuth();
  const [view, setView] = useState(investigation);
  const [busy, setBusy] = useState(false);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const [fixPreview, setFixPreview] = useState<CausalFixPreview | undefined>(undefined);
  const idempotencyKey = useRef<string | null>(null);
  const operationAvailable = Boolean(features.causal_remediation_investigation);
  const previewAvailable = Boolean(features.causal_remediation_fix_preview);
  const groupID = group.id;
  const groupHash = group.content_hash;
  const operationRef = useMemo<CausalRemediationRef | null>(
    () =>
      jobID && patternID && patternHash && groupID && groupHash
        ? { jobID, patternID, patternHash, causalGroupID: groupID, causalGroupHash: groupHash }
        : null,
    [groupHash, groupID, jobID, patternHash, patternID],
  );

  const blocked = causalRemediationBlockedReason(group, view, operationAvailable, patternEligible);
  const presentation = patternRemediationPresentation(view);
  const message = blocked ? blocked.message : presentation.message;
  const active = activeStates.has(presentation.state);
  const addressable = Boolean(operationRef);
  // A blocked cause never polls: the server would reject the operation, so
  // asking for its status would be a request that can only ever fail.
  const pollable = !blocked && addressable && operationAvailable && authStatus === "authenticated";

  useEffect(() => {
    setView(investigation);
  }, [investigation]);

  useEffect(() => {
    if (!active) idempotencyKey.current = null;
  }, [active]);

  useEffect(() => {
    if (!pollable || !operationRef) return;
    let cancelled = false;
    void getCausalRemediationStatus(operationRef)
      .then((status) => {
        // The public not-investigated projection remains the safe fallback.
        if (!cancelled) setView(status);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [operationRef, pollable]);

  useEffect(() => {
    if (!pollable || !operationRef || !active) return;
    const timer = window.setInterval(() => {
      void getCausalRemediationStatus(operationRef)
        .then(setView)
        .catch(() => undefined);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [active, operationRef, pollable]);

  const start = async (refresh: boolean) => {
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    if (authStatus !== "authenticated" || !operationRef) return;
    if (!idempotencyKey.current) idempotencyKey.current = crypto.randomUUID();
    setBusy(true);
    setError(undefined);
    try {
      setView(await startCausalRemediation(operationRef, idempotencyKey.current, refresh));
    } catch (failure) {
      idempotencyKey.current = null;
      setError(failure instanceof Error ? failure.message : "Investigation could not start.");
    } finally {
      setBusy(false);
    }
  };

  const preview = async () => {
    if (authStatus !== "authenticated" || !operationRef) return;
    setPreviewBusy(true);
    setError(undefined);
    try {
      setFixPreview(await previewCausalFix(operationRef, crypto.randomUUID()));
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "Fix preview generation failed.");
    } finally {
      setPreviewBusy(false);
    }
  };

  // A blocked group never ran an operation, so it has nothing to disclose.
  const details = blocked ? undefined : investigationDetails(view, error, message);
  const canStart = !blocked && operationAvailable &&
    (presentation.state === "not_investigated" || presentation.state === "failed") &&
    authStatus !== "loading";
  const canPreview = !blocked && previewAvailable && authStatus === "authenticated" &&
    presentation.state === "actionable" && Boolean(view?.target);

  // A missing deployment capability is not a verdict about this cause, so it
  // gets its own chip treatment rather than sharing the outlined verdict style.
  const capabilityBlocked = blocked?.scope === "deployment";

  return (
    <Box aria-live="polite" sx={{ mt: 1.5 }}>
      <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
        <Typography color="textSecondary" component="h5" sx={{ ...overviewTypography.eyebrow, m: 0 }}>
          Verified fix investigation
        </Typography>
        <Chip
          label={blocked ? blocked.label : presentation.label}
          size="small"
          icon={capabilityBlocked ? <CloudOff aria-hidden /> : undefined}
          color={!blocked && presentation.state === "actionable" ? "success" : "default"}
          variant={capabilityBlocked ? "filled" : "outlined"}
          sx={capabilityBlocked ? { bgcolor: "action.disabledBackground", color: "text.secondary" } : undefined}
        />
      </Stack>
      <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.secondaryBody }}>
        {message}
      </Typography>
      {blocked && chatAvailable && (
        // The row reports one mechanism, so a block here is not the end of the
        // road. Name the path that stays open rather than stopping at a verdict
        // that reads as if nothing can be done about this cause. The chat picks
        // its own evidence builds, so this promises a place to ask, not the same
        // evidence this investigation would have read.
        <Typography color="textSecondary" sx={{ mt: 0.5, ...overviewTypography.description }}>
          You can still ask about this cause in the pattern chat below.
        </Typography>
      )}
      {canStart && (
        <Button
          size="small"
          variant="outlined"
          disabled={busy}
          onClick={() => void start(presentation.state === "failed")}
          sx={{ mt: 1 }}
        >
          {authStatus === "anonymous" ? "Sign in to investigate" : "Investigate possible fix"}
        </Button>
      )}
      {canPreview && (
        <Box sx={{ mt: 1 }}>
          <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
            <Button size="small" variant="contained" disabled={previewBusy} onClick={() => void preview()}>
              Preview Fix PR
            </Button>
            <Chip size="small" color="warning" variant="outlined" label="Experimental" />
          </Stack>
          <Typography color="textSecondary" sx={{ mt: 0.5 }}>Generates a review preview only. No GitHub PR will be created.</Typography>
        </Box>
      )}
      {fixPreview && (
        <Box sx={{ mt: 1, border: "1px solid", borderColor: "divider", borderRadius: 1, p: 1.5 }}>
          <Typography sx={{ fontWeight: 700 }}>{fixPreview.summary}</Typography>
          <Typography color="textSecondary">Base: {fixPreview.base_revision}</Typography>
          <Typography color="textSecondary">Changed files: {fixPreview.changed_files.join(", ")}</Typography>
          <Box component="pre" sx={{ overflowX: "auto", whiteSpace: "pre", fontSize: 12, mt: 1 }}>{fixPreview.diff}</Box>
        </Box>
      )}
      {details && (
        <Accordion
          disableGutters
          elevation={0}
          sx={{
            mt: 1,
            border: "1px solid",
            borderColor: "divider",
            borderRadius: "4px",
            "&::before": { display: "none" },
          }}
        >
          <AccordionSummary expandIcon={<ExpandMore aria-hidden />}>
            <Typography sx={overviewTypography.data}>Investigation details</Typography>
          </AccordionSummary>
          <AccordionDetails sx={{ pt: 0 }}>
            <Typography color="textSecondary" sx={{ whiteSpace: "pre-line", overflowWrap: "anywhere" }}>
              {details}
            </Typography>
          </AccordionDetails>
        </Accordion>
      )}
    </Box>
  );
}

function investigationDetails(
  investigation?: PatternRemediationInvestigationSummary,
  localError?: string,
  conciseMessage?: string,
): string | undefined {
  const details: string[] = [];
  if (investigation?.reason && investigation.reason !== conciseMessage) details.push(investigation.reason);
  if (investigation?.target) details.push(formatTarget(investigation.target));
  if (investigation?.completed_at) details.push(`Completed: ${investigation.completed_at}`);
  if (localError) details.push(localError);
  return details.length > 0 ? details.join("\n") : undefined;
}

function formatTarget(target: PatternRemediationTargetSummary): string {
  const identity = [target.symbol, target.required_call, target.job, target.container, target.name]
    .filter(Boolean)
    .join(" · ");
  const value = target.value ? ` = ${target.value}` : "";
  return `Verified target: ${target.kind} · ${target.repository}@${target.revision} · ${target.path}${identity ? ` · ${identity}` : ""}${value}`;
}
