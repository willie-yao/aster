import { useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { ExpandMore } from "@mui/icons-material";
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
import { patternRemediationPresentation } from "../lib/patternRemediation";
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

export function PatternRemediation({
  groups,
  investigations,
  jobID,
  patternID,
  patternHash,
}: {
  groups: PatternCausalGroup[];
  investigations?: PatternRemediationInvestigationSummary[];
  jobID?: string;
  patternID?: string;
  patternHash?: string;
}) {
  const { features } = useCapabilities();
  const { status: authStatus, signIn } = useAuth();
  const recurringGroups = useMemo(() => groups.filter((group) => group.builds.length >= 2), [groups]);
  const [states, setStates] = useState<Map<string, PatternRemediationInvestigationSummary>>(
    () => new Map(investigations?.map((item) => [item.causal_group_hash, item])),
  );
  const [busyHash, setBusyHash] = useState<string | null>(null);
  const [errors, setErrors] = useState<Map<string, string>>(new Map());
  const [previews, setPreviews] = useState<Map<string, CausalFixPreview>>(new Map());
  const [previewBusyHash, setPreviewBusyHash] = useState<string | null>(null);
  const idempotencyKeys = useRef(new Map<string, string>());
  const operationAvailable = Boolean(features.causal_remediation_investigation);
  const previewAvailable = Boolean(features.causal_remediation_fix_preview);

  useEffect(() => {
    setStates(new Map(investigations?.map((item) => [item.causal_group_hash, item])));
  }, [investigations]);

  useEffect(() => {
    if (!operationAvailable || authStatus !== "authenticated" || !jobID || !patternID || !patternHash) return;
    let cancelled = false;
    const load = async (group: PatternCausalGroup) => {
      const ref = operationRef(jobID, patternID, patternHash, group);
      if (!ref) return;
      try {
        const view = await getCausalRemediationStatus(ref);
        if (!cancelled) updateState(setStates, view);
      } catch {
        // The public not-investigated projection remains the safe fallback.
      }
    };
    void Promise.all(recurringGroups.map(load));
    return () => {
      cancelled = true;
    };
  }, [authStatus, jobID, operationAvailable, patternHash, patternID, recurringGroups]);

  useEffect(() => {
    for (const [hash, view] of states) {
      if (!activeStates.has(view.state)) idempotencyKeys.current.delete(hash);
    }
  }, [states]);

  useEffect(() => {
    if (!operationAvailable || authStatus !== "authenticated" || !jobID || !patternID || !patternHash) return;
    const active = recurringGroups.filter((group) => {
      const state = group.content_hash ? states.get(group.content_hash)?.state : undefined;
      return state ? activeStates.has(state) : false;
    });
    if (active.length === 0) return;
    const timer = window.setInterval(() => {
      for (const group of active) {
        const ref = operationRef(jobID, patternID, patternHash, group);
        if (!ref) continue;
        void getCausalRemediationStatus(ref)
          .then((view) => updateState(setStates, view))
          .catch(() => undefined);
      }
    }, 1500);
    return () => window.clearInterval(timer);
  }, [authStatus, jobID, operationAvailable, patternHash, patternID, recurringGroups, states]);

  const start = async (group: PatternCausalGroup, refresh: boolean) => {
    if (authStatus === "anonymous") {
      signIn();
      return;
    }
    if (authStatus !== "authenticated" || !jobID || !patternID || !patternHash) return;
    const ref = operationRef(jobID, patternID, patternHash, group);
    if (!ref) return;
    let requestID = idempotencyKeys.current.get(ref.causalGroupHash);
    if (!requestID) {
      requestID = crypto.randomUUID();
      idempotencyKeys.current.set(ref.causalGroupHash, requestID);
    }
    setBusyHash(ref.causalGroupHash);
    setErrors((current) => withoutKey(current, ref.causalGroupHash));
    try {
      const view = await startCausalRemediation(ref, requestID, refresh);
      updateState(setStates, view);
    } catch (error) {
      idempotencyKeys.current.delete(ref.causalGroupHash);
      setErrors((current) => new Map(current).set(ref.causalGroupHash, error instanceof Error ? error.message : "Investigation could not start."));
    } finally {
      setBusyHash(null);
    }
  };

  const preview = async (group: PatternCausalGroup) => {
    if (authStatus !== "authenticated" || !jobID || !patternID || !patternHash) return;
    const ref = operationRef(jobID, patternID, patternHash, group);
    if (!ref) return;
    setPreviewBusyHash(ref.causalGroupHash);
    setErrors((current) => withoutKey(current, ref.causalGroupHash));
    try {
      const result = await previewCausalFix(ref, crypto.randomUUID());
      setPreviews((current) => new Map(current).set(ref.causalGroupHash, result));
    } catch (error) {
      setErrors((current) => new Map(current).set(ref.causalGroupHash, error instanceof Error ? error.message : "Fix preview generation failed."));
    } finally {
      setPreviewBusyHash(null);
    }
  };

  return (
    <Box aria-live="polite">
      <Typography
        component="h3"
        color="text.secondary"
        sx={{ ...overviewTypography.subsectionHeading, fontSize: "14px", lineHeight: "20px" }}
      >
        Remediation
      </Typography>
      {recurringGroups.length === 0 ? (
        <Typography sx={{ mt: 0.75 }}>
          No recurring causal group was identified for remediation investigation.
        </Typography>
      ) : (
        <Stack spacing={1.5} sx={{ mt: 0.75 }}>
          {recurringGroups.map((group, index) => {
            const investigation = group.content_hash ? states.get(group.content_hash) : undefined;
            const presentation = patternRemediationPresentation(investigation);
            const localError = group.content_hash ? errors.get(group.content_hash) : undefined;
            const details = investigationDetails(investigation, localError, presentation.message);
            const fixPreview = group.content_hash ? previews.get(group.content_hash) : undefined;
            const canPreview = previewAvailable && authStatus === "authenticated" && presentation.state === "actionable" && Boolean(investigation?.target);
            const canStart = operationAvailable &&
              (presentation.state === "not_investigated" || presentation.state === "failed") &&
              authStatus !== "loading";
            return (
              <Box key={group.id ?? group.content_hash ?? `${group.builds.join("-")}-${index}`}>
                <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
                  {recurringGroups.length > 1 && (
                    <Typography sx={{ ...overviewTypography.data, fontWeight: 700 }}>
                      Cause {index + 1}
                    </Typography>
                  )}
                  <Chip
                    label={presentation.label}
                    size="small"
                    color={presentation.state === "actionable" ? "success" : "default"}
                    variant="outlined"
                  />
                </Stack>
                <Typography sx={{ mt: 0.75 }}>{presentation.message}</Typography>
                {canStart && (
                  <Button
                    size="small"
                    variant="outlined"
                    disabled={busyHash === group.content_hash}
                    onClick={() => void start(group, presentation.state === "failed")}
                    sx={{ mt: 1 }}
                  >
                    {authStatus === "anonymous" ? "Sign in to investigate" : "Investigate possible fix"}
                  </Button>
                )}
                {canPreview && (
                  <Box sx={{ mt: 1 }}>
                    <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
                      <Button size="small" variant="contained" disabled={previewBusyHash === group.content_hash} onClick={() => void preview(group)}>
                        Preview Fix PR
                      </Button>
                      <Chip size="small" color="warning" variant="outlined" label="Experimental" />
                    </Stack>
                    <Typography color="text.secondary" sx={{ mt: 0.5 }}>Generates a review preview only. No GitHub PR will be created.</Typography>
                  </Box>
                )}
                {fixPreview && (
                  <Box sx={{ mt: 1, border: "1px solid", borderColor: "divider", borderRadius: 1, p: 1.5 }}>
                    <Typography sx={{ fontWeight: 700 }}>{fixPreview.summary}</Typography>
                    <Typography color="text.secondary">Base: {fixPreview.base_revision}</Typography>
                    <Typography color="text.secondary">Changed files: {fixPreview.changed_files.join(", ")}</Typography>
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
                      <Typography color="text.secondary" sx={{ whiteSpace: "pre-line", overflowWrap: "anywhere" }}>
                        {details}
                      </Typography>
                    </AccordionDetails>
                  </Accordion>
                )}
              </Box>
            );
          })}
        </Stack>
      )}
    </Box>
  );
}

function operationRef(jobID: string, patternID: string, patternHash: string, group: PatternCausalGroup): CausalRemediationRef | null {
  if (!group.id || !group.content_hash) return null;
  return {
    jobID,
    patternID,
    patternHash,
    causalGroupID: group.id,
    causalGroupHash: group.content_hash,
  };
}

function updateState(
  setStates: Dispatch<SetStateAction<Map<string, PatternRemediationInvestigationSummary>>>,
  view: PatternRemediationInvestigationSummary,
) {
  setStates((current) => new Map(current).set(view.causal_group_hash, view));
}

function withoutKey(values: Map<string, string>, key: string): Map<string, string> {
  const next = new Map(values);
  next.delete(key);
  return next;
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
