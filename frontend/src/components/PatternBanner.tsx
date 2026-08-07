import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { AutoAwesome } from "@mui/icons-material";
import type {
  BuildResult,
  PatternAnalysis,
  PatternRefreshStatus,
  RemediationObservation,
} from "../types/dashboard";
import type { AnalysisChatReference } from "../types/analysisChat";
import {
  fileSortKey,
  fileToUrl,
  meetsConfidenceFloor,
  type FileToUrlContext,
} from "../lib/utils";
import { RichText } from "./RichText";
import { FailureActions } from "./FailureActions";
import { useRemediations, useResolved } from "../hooks/useData";
import { soft } from "../theme";
import { AnalysisChat } from "./AnalysisChat";
import { useCapabilities } from "../hooks/useCapabilities";
import { patternChatAvailability } from "../lib/patternChat";
import { patternActionEligibilityHint } from "../lib/actionEligibility";
import { jobRunPath } from "../lib/routes";
import { AnalysisBriefing } from "./AnalysisBriefing";
import { overviewTypography } from "../theme/overview";

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
  ) {
    return "error";
  }
  if (status === "inconclusive") return "warning";
  return "info";
}

function firstSentence(value: string): string {
  const match = value.trim().match(/^.*?[.!?](?:\s|$)/u);
  return match?.[0].trim() || value.trim();
}

function BriefingSection({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <Box>
      <Typography component="h3" color="text.secondary" sx={overviewTypography.subsectionHeading}>
        {label}
      </Typography>
      <Box sx={{ mt: 0.5, ...overviewTypography.primaryBody }}>{children}</Box>
    </Box>
  );
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
    pattern.shared_builds?.length &&
      pattern.shared_builds.every((buildID) => runs.some((run) => run.build_id === buildID)),
  );
  const chatAvailability = patternChatAvailability(
    pattern,
    jobID,
    hasEvidenceBuild,
    Boolean(features.analysis_chat),
  );
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
  const patternFileCtx = {
    builds: buildContexts,
    fileLinks: pattern.file_links,
  } satisfies FileToUrlContext;
  const isCurrent = !refreshStatus || refreshStatus.state === "current";
  const actionEligibility = patternActionEligibilityHint(
    pattern.remediation_targets,
    attempt?.status,
  );
  const fixPatterns =
    isCurrent &&
    pattern.id &&
    pattern.content_hash &&
    pattern.suggested_fix &&
    meetsConfidenceFloor(pattern.confidence, features.chat_fix_min_confidence ?? "high")
      ? [pattern]
      : [];
  const patternLabel = pattern.systemic ? "Recurring pattern" : "No shared root cause";
  const metadata = `${pattern.builds_analyzed} ${pattern.builds_analyzed === 1 ? "build" : "builds"} · ${pattern.confidence} confidence`;
  const staleNotice = refreshStatus && refreshStatus.state !== "current" ? (
    <Alert severity="warning" variant="outlined" sx={{ borderRadius: "4px" }}>
      Last known good pattern from {refreshStatus.last_successful_at ?? "an earlier refresh"}.
      Current refresh: {refreshStatus.failure_category ?? refreshStatus.state}.
    </Alert>
  ) : null;

  const details = (
    <>
      {(resolvedEntry || attempt) && (
        <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
          {resolvedEntry && (
            <Chip
              size="small"
              label="Resolved"
              sx={{
                borderRadius: "4px",
                fontWeight: 650,
                bgcolor: (theme) => soft(theme, "success", 0.16),
                color: "success.main",
              }}
            />
          )}
          {attempt && (
            <Chip
              size="small"
              label={remediationStatusLabel(attempt.status)}
              sx={{
                borderRadius: "4px",
                fontWeight: 650,
                bgcolor: (theme) => soft(theme, remediationStatusColor(attempt.status), 0.16),
                color: `${remediationStatusColor(attempt.status)}.main`,
              }}
            />
          )}
        </Stack>
      )}

      {resolvedEntry && (
        <Typography color="text.secondary" sx={overviewTypography.description}>
          Marked resolved by {resolvedEntry.resolved_by}
          {resolvedEntry.note ? `. ${resolvedEntry.note}` : ""}. It reopens automatically if it recurs.
        </Typography>
      )}

      {attempt && (
        <BriefingSection label="Remediation status">
          <Typography component="p" sx={{ m: 0, ...overviewTypography.secondaryBody }}>
            Attempt {attempt.number}: {remediationStatusLabel(attempt.status)}
            {attempt.outcome_reason ? `. ${attempt.outcome_reason}` : ""}
          </Typography>
          <Stack direction="row" spacing={2} sx={{ mt: 0.5, flexWrap: "wrap", rowGap: 0.5 }}>
            <Link href={attempt.url} target="_blank" rel="noreferrer">
              Pull request #{attempt.pr_number}
            </Link>
            {remediation?.issue && (
              <Link href={remediation.issue.url} target="_blank" rel="noreferrer">
                Issue #{remediation.issue.number}
              </Link>
            )}
            {latestObservation?.prow_url && (
              <Link href={latestObservation.prow_url} target="_blank" rel="noreferrer">
                Latest Prow observation
              </Link>
            )}
          </Stack>
        </BriefingSection>
      )}

      {staleNotice}

      {pattern.systemic && pattern.shared_root_cause && (
        <BriefingSection label="Root cause">
          <RichText text={pattern.shared_root_cause} steps fileCtx={patternFileCtx} />
        </BriefingSection>
      )}

      {pattern.systemic && pattern.suggested_fix && (
        <BriefingSection label="Suggested remediation">
          <RichText text={pattern.suggested_fix} steps fileCtx={patternFileCtx} />
        </BriefingSection>
      )}

      {pattern.source_ref && (
        <BriefingSection label="Source grounding">
          <Typography component="code" sx={{ ...overviewTypography.data, overflowWrap: "anywhere" }}>
            {pattern.source_ref}
          </Typography>
        </BriefingSection>
      )}

      {pattern.systemic && pattern.shared_builds && pattern.shared_builds.length > 0 && (
        <BriefingSection label="Affected builds">
          <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap", rowGap: 1 }}>
            {pattern.shared_builds.map((buildID) => (
              <Link
                key={buildID}
                component={RouterLink}
                to={jobID ? jobRunPath(jobID, buildID) : "#"}
                underline="none"
                sx={{
                  minHeight: 32,
                  display: "inline-flex",
                  alignItems: "center",
                  px: 0.75,
                  borderRadius: "4px",
                  bgcolor: "action.selected",
                  color: "primary.main",
                  ...overviewTypography.data,
                  "&:hover": { bgcolor: "surface.containerHigh" },
                  "&:focus-visible": {
                    outline: "2px solid",
                    outlineColor: "primary.main",
                    outlineOffset: 2,
                  },
                }}
              >
                {buildID}
              </Link>
            ))}
          </Stack>
        </BriefingSection>
      )}

      {pattern.relevant_files && pattern.relevant_files.length > 0 && (
        <BriefingSection label="Related files">
          <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
            {[...pattern.relevant_files]
              .sort((left, right) => fileSortKey(left, patternFileCtx) - fileSortKey(right, patternFileCtx))
              .map((file) => {
                const url = fileToUrl(file, patternFileCtx);
                return (
                  <Box component="li" key={file} sx={{ py: 0.25, ...overviewTypography.data }}>
                    {url ? (
                      <Link href={url} target="_blank" rel="noopener noreferrer">
                        {file}
                      </Link>
                    ) : (
                      file
                    )}
                  </Box>
                );
              })}
          </Box>
        </BriefingSection>
      )}

      {chatAvailability === "stale" && (
        <Alert severity="info" variant="outlined" sx={{ borderRadius: "4px" }}>
          Recurring-pattern chat is unavailable because this dashboard data predates content hashing.
          Refresh the dashboard data to enable it.
        </Alert>
      )}
    </>
  );

  const actions = chatRef || (isCurrent && pattern.systemic && pattern.id) ? (
    <Stack spacing={1.25}>
      {chatRef && (
        <AnalysisChat
          key={`${chatRef.job_id}\u0000${chatRef.pattern_id}\u0000${chatRef.pattern_hash}`}
          analysisRef={chatRef}
          fileCtx={{ builds: buildContexts }}
          fixPatterns={fixPatterns}
          appearance="detail"
        />
      )}
      {isCurrent && pattern.systemic && pattern.id && (
        <FailureActions
          failureID={pattern.id}
          eligibilityHint={actionEligibility}
          appearance="detail"
        />
      )}
    </Stack>
  ) : undefined;

  return (
    <AnalysisBriefing
      id={pattern.id ? `pattern-${pattern.id}` : undefined}
      title="Analysis briefing"
      mobileTitle={patternLabel}
      icon={<AutoAwesome aria-hidden sx={{ fontSize: 18, color: "primary.main" }} />}
      metadata={`${patternLabel} · ${metadata}`}
      mobileMetadata={staleNotice ? `Last known good · ${metadata}` : metadata}
      mobileNotice={staleNotice}
      summary={<RichText text={pattern.summary} steps fileCtx={patternFileCtx} />}
      mobileSynopsis={firstSentence(pattern.shared_root_cause ?? pattern.summary)}
      details={details}
      actions={actions}
    />
  );
}
