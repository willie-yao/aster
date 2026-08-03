import Box from "@mui/material/Box";
import Alert from "@mui/material/Alert";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { Insights } from "@mui/icons-material";
import type { BuildResult, PatternAnalysis, PatternRefreshStatus, RemediationObservation } from "../types/dashboard";
import type { AnalysisChatReference } from "../types/analysisChat";
import { confidenceColor, meetsConfidenceFloor, type FileToUrlContext } from "../lib/utils";
import { RichText } from "./RichText";
import { LabeledBlock } from "./LabeledBlock";
import { FailureActions } from "./FailureActions";
import { useRemediations, useResolved } from "../hooks/useData";
import { soft } from "../theme";
import { AnalysisChat } from "./AnalysisChat";
import { useCapabilities } from "../hooks/useCapabilities";
import { patternChatAvailability } from "../lib/patternChat";
import { jobRunPath } from "../lib/routes";

function remediationStatusLabel(status: string): string {
  return status.replaceAll("_", " ");
}

function remediationStatusColor(status: string): "success" | "warning" | "error" | "info" {
  if (status === "verified_fixed" || status === "premerge_verified") return "success";
  if (
    status === "still_failing_same_cause" ||
    status === "failing_different_cause" ||
    status === "presubmit_failed_same_cause" ||
    status === "presubmit_failed_different_cause"
  )
    return "error";
  if (status === "inconclusive") return "warning";
  return "info";
}

export function PatternBanner({
  pattern,
  jobID,
  runs = [],
  refreshStatus,
}: {
  pattern: PatternAnalysis;
  jobID?: string;
  runs?: BuildResult[];
  refreshStatus?: PatternRefreshStatus;
}) {
  const color = pattern.systemic ? "warning" : "success";
  const confColor = confidenceColor(pattern.confidence, color);

  const { data: resolved } = useResolved();
  const { data: remediations } = useRemediations();
  const { features } = useCapabilities();
  const resolvedEntry = pattern.id ? resolved.resolved[pattern.id] : undefined;
  const remediation = pattern.id ? remediations.remediations[pattern.id] : undefined;
  const attempt = remediation?.attempt;
  const latestObservation = attempt?.observations?.reduce((latest, observation) => {
    if (!latest) return observation;
    const buildOrder = observation.build_id.localeCompare(latest.build_id, undefined, { numeric: true });
    if (buildOrder !== 0) return buildOrder > 0 ? observation : latest;
    const observedAt = observation.completed_at ?? observation.started_at ?? "";
    const latestObservedAt = latest.completed_at ?? latest.started_at ?? "";
    return observedAt > latestObservedAt ? observation : latest;
  }, undefined as RemediationObservation | undefined);
  const hasEvidenceBuild = Boolean(
    pattern.shared_builds?.length && pattern.shared_builds.every((buildID) => runs.some((run) => run.build_id === buildID)),
  );
  const chatAvailability = patternChatAvailability(pattern, jobID, hasEvidenceBuild, Boolean(features.analysis_chat));
  const chatRef: AnalysisChatReference | null =
    chatAvailability === "ready" && pattern.id && pattern.content_hash && jobID
      ? {
          scope: "pattern",
          job_id: jobID,
          pattern_id: pattern.id,
          pattern_hash: pattern.content_hash,
        }
      : null;
  const buildContexts = Object.fromEntries(
    runs.map((run) => [
      run.build_id,
      { buildLogUrl: run.build_log_url, webUrl: run.web_url } satisfies FileToUrlContext,
    ]),
  );
  const patternFileCtx = { builds: buildContexts, fileLinks: pattern.file_links } satisfies FileToUrlContext;
  const isCurrent = !refreshStatus || refreshStatus.state === "current";
  const fixPatterns =
    isCurrent && pattern.id && pattern.content_hash && pattern.suggested_fix &&
    meetsConfidenceFloor(pattern.confidence, features.chat_fix_min_confidence ?? "high")
      ? [pattern]
      : [];

  return (
    <Box
      id={pattern.id ? `pattern-${pattern.id}` : undefined}
      component="section"
      className="ai-aurora"
      sx={{
        borderRadius: "12px",
        bgcolor: (t) => soft(t, color, 0.05),
        p: { xs: 2, sm: 2.5 },
      }}
    >
      <Stack spacing={2}>
        <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap" }}>
          <Insights sx={{ fontSize: 20, color: `${color}.main` }} />
          <Typography variant="label" sx={{ fontWeight: 600 }} color={`${color}.main`}>
            {pattern.systemic ? "Recurring failure pattern" : "No shared root cause"}
          </Typography>
          <Chip
            size="small"
            label={`${pattern.builds_analyzed} builds analyzed`}
            sx={{ bgcolor: "action.selected", color: "text.secondary", fontWeight: 600 }}
          />
          <Chip
            size="small"
            label={`Confidence: ${pattern.confidence}`}
            sx={{
              fontWeight: 600,
              ...(confColor
                ? { bgcolor: (t) => soft(t, confColor, 0.2), color: `${confColor}.main` }
                : { bgcolor: "action.selected", color: "text.secondary" }),
            }}
          />
          {resolvedEntry && (
            <Chip
              size="small"
              label="Resolved"
              sx={{
                fontWeight: 600,
                bgcolor: (t) => soft(t, "success", 0.2),
                color: "success.main",
              }}
            />
          )}
          {attempt && (
            <Chip
              size="small"
              label={remediationStatusLabel(attempt.status)}
              sx={{
                fontWeight: 600,
                bgcolor: (t) => soft(t, remediationStatusColor(attempt.status), 0.2),
                color: `${remediationStatusColor(attempt.status)}.main`,
              }}
            />
          )}
        </Stack>

        {resolvedEntry && (
          <Typography variant="caption" color="text.secondary">
            Marked resolved by {resolvedEntry.resolved_by}
            {resolvedEntry.note ? ` — ${resolvedEntry.note}` : ""}. Re-opens
            automatically if it recurs.
          </Typography>
        )}

        {attempt && (
          <Stack spacing={0.5}>
            <Typography variant="caption" color="text.secondary">
              Remediation attempt {attempt.number}: {remediationStatusLabel(attempt.status)}
              {attempt.outcome_reason ? `. ${attempt.outcome_reason}` : ""}
            </Typography>
            <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap", rowGap: 0.5 }}>
              <Link href={attempt.url} target="_blank" rel="noreferrer" variant="caption">
                Pull request #{attempt.pr_number}
              </Link>
              {remediation?.issue && (
                <Link href={remediation.issue.url} target="_blank" rel="noreferrer" variant="caption">
                  Issue #{remediation.issue.number}
                </Link>
              )}
              {latestObservation?.prow_url && (
                <Link href={latestObservation.prow_url} target="_blank" rel="noreferrer" variant="caption">
                  Latest Prow observation
                </Link>
              )}
            </Stack>
          </Stack>
        )}

        {refreshStatus && refreshStatus.state !== "current" && (
          <Alert severity="warning" variant="outlined">
            Last known good pattern from {refreshStatus.last_successful_at ?? "an earlier refresh"}. Current refresh: {refreshStatus.failure_category ?? refreshStatus.state}.
          </Alert>
        )}

        <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
          <RichText text={pattern.summary} steps fileCtx={patternFileCtx} />
        </Typography>

        {pattern.systemic && pattern.shared_root_cause && (
          <LabeledBlock label="Shared Root Cause" accent={color}>
            <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
              <RichText text={pattern.shared_root_cause} steps fileCtx={patternFileCtx} />
            </Typography>
          </LabeledBlock>
        )}

        {pattern.systemic && pattern.suggested_fix && (
          <LabeledBlock label="Suggested Fix" accent="primary">
            <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
              <RichText text={pattern.suggested_fix} steps fileCtx={patternFileCtx} />
            </Typography>
          </LabeledBlock>
        )}

        {pattern.source_ref && (
          <Typography variant="caption" color="text.secondary">
            Source grounding: {pattern.source_ref}
          </Typography>
        )}

        {pattern.systemic && pattern.shared_builds && pattern.shared_builds.length > 0 && (
          <Box>
            <Typography variant="label" color="text.secondary" sx={{ fontWeight: 600, display: "block", mb: 0.5 }}>
              Affected Builds
            </Typography>
            <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap", rowGap: 1 }}>
              {pattern.shared_builds.map((b) => (
                <Link
                  key={b}
                  component={RouterLink}
                  to={jobID ? jobRunPath(jobID, b) : "#"}
                  underline="none"
                  sx={{
                    fontFamily: "monospace",
                    fontSize: "0.8125rem",
                    px: 0.75,
                    py: 0.25,
                    borderRadius: "4px",
                    bgcolor: "action.selected",
                    color: "primary.main",
                    "&:hover": { bgcolor: (t) => soft(t, "primary", 0.15) },
                  }}
                >
                  {b}
                </Link>
              ))}
            </Stack>
          </Box>
        )}

        {chatAvailability === "stale" && (
          <Alert severity="info" variant="outlined">
            Recurring-pattern chat is unavailable because this dashboard data predates content hashing. Refresh the dashboard data to enable it.
          </Alert>
        )}

        {chatRef && (
          <AnalysisChat
            key={`${chatRef.job_id}\u0000${chatRef.pattern_id}\u0000${chatRef.pattern_hash}`}
            analysisRef={chatRef}
            fileCtx={{ builds: buildContexts }}
            fixPatterns={fixPatterns}
          />
        )}

        {isCurrent && pattern.systemic && pattern.id && <FailureActions failureID={pattern.id} />}
      </Stack>
    </Box>
  );
}
