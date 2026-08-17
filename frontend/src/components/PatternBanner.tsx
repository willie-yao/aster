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
import { useResolved } from "../hooks/useData";
import { AnalysisChat } from "./AnalysisChat";
import { useCapabilities } from "../hooks/useCapabilities";
import { patternChatAvailability, patternChatHasEvidenceBuild } from "../lib/patternChat";
import { patternActionEligibilityHint, patternLifecycleActive } from "../lib/actionEligibility";
import { jobRunPath } from "../lib/routes";
import { AnalysisBriefing } from "./AnalysisBriefing";
import { overviewTypography } from "../theme/overview";
import { PatternRemediation } from "./PatternRemediation";
import { PatternFixGuidance } from "./PatternFixGuidance";
import { patternFixGuidanceBuildID } from "../lib/patternFixGuidance";

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
      <Typography
        component="h3"
        color="text.secondary"
        sx={{ ...overviewTypography.subsectionHeading, fontSize: "14px", lineHeight: "20px" }}
      >
        {label}
      </Typography>
      <Box sx={{ mt: 0.75, fontSize: "16px", lineHeight: "25px" }}>{children}</Box>
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
  const { features } = useCapabilities();
  const analysisOnly = Boolean(pattern.recurrence_classification);
  const fixGuidanceBuildID = patternFixGuidanceBuildID(pattern, runs);
  const showFixGuidance = Boolean(jobID && fixGuidanceBuildID);
  const resolvedEntry = !analysisOnly && pattern.id ? resolved.resolved[pattern.id] : undefined;
  const hasEvidenceBuild = patternChatHasEvidenceBuild(
    pattern,
    runs.map((run) => run.build_id),
    Boolean(refreshStatus && refreshStatus.state !== "current"),
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
  const lifecycle = pattern.lifecycle;
  const lifecycleActive = patternLifecycleActive(lifecycle);
  const actionEligibility = patternActionEligibilityHint(
    pattern.remediation_targets,
    lifecycle,
    pattern.systemic,
    refreshStatus,
  );
  const fixPatterns =
    !analysisOnly &&
    isCurrent &&
    lifecycleActive &&
    pattern.id &&
    pattern.content_hash &&
    pattern.suggested_fix &&
    meetsConfidenceFloor(pattern.confidence, features.chat_fix_min_confidence ?? "high")
      ? [pattern]
      : [];
  const recurrenceLabel = pattern.recurrence_classification === "shared_cause"
    ? "Recurring shared cause"
    : pattern.recurrence_classification === "mixed_causes"
      ? "Multiple recurring causes"
      : pattern.recurrence_classification === "unrelated"
        ? "Unrelated failures"
        : pattern.recurrence_classification === "insufficient_evidence"
          ? "Insufficient evidence"
          : pattern.systemic ? "Recurring pattern" : "No shared root cause";
  const patternLabel = lifecycle?.state === "verified_fixed"
    ? "Verified fixed"
    : lifecycle?.state === "observing"
      ? "Fix verification"
      : lifecycle?.state === "recovered"
        ? "Watching recovery"
        : recurrenceLabel;
  const metadata = `${pattern.builds_analyzed} ${pattern.builds_analyzed === 1 ? "build" : "builds"} · ${pattern.confidence} confidence`;
  const staleNotice = refreshStatus && refreshStatus.state !== "current" ? (
    <Alert severity="warning" variant="outlined" sx={{ borderRadius: "4px" }}>
      Pattern from the last successful refresh at {refreshStatus.last_successful_at ?? "an earlier time"}.
      Current refresh status: {refreshStatus.failure_category ?? refreshStatus.state}.
    </Alert>
  ) : null;
  const lifecycleNotice = lifecycle && !lifecycleActive ? (
    <Alert
      severity={lifecycle.state === "verified_fixed" ? "success" : "info"}
      variant="outlined"
      sx={{ borderRadius: "4px" }}
    >
      <Typography variant="body2" sx={{ fontWeight: 700 }}>
        {lifecycle.state === "verified_fixed"
          ? "Verified fixed"
          : lifecycle.state === "recovered"
            ? "Watching recovery"
            : "Remediation present, verifying the fix"}
      </Typography>
      <Typography variant="body2">{lifecycle.reason}</Typography>
      {lifecycle.source_revision && (
        <Typography component="div" variant="caption" sx={{ mt: 0.75, overflowWrap: "anywhere" }}>
          Verified remediation source: <code>{pattern.remediation_verification?.repository
            ? `${pattern.remediation_verification.repository}@${lifecycle.source_revision}`
            : lifecycle.source_revision}</code>
        </Typography>
      )}
      {jobID && lifecycle.passing_builds && lifecycle.passing_builds.length > 0 && (
        <Stack direction="row" spacing={1} sx={{ mt: 0.75, alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
          <Typography variant="caption" color="text.secondary">Verified passing runs:</Typography>
          {lifecycle.passing_builds.map((buildID) => (
            <Link key={buildID} component={RouterLink} to={jobRunPath(jobID, buildID)} sx={overviewTypography.data}>
              {buildID}
            </Link>
          ))}
        </Stack>
      )}
      {jobID && lifecycle.recovery_builds && lifecycle.recovery_builds.length > 0 && (
        <Stack direction="row" spacing={1} sx={{ mt: 0.75, alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
          <Typography variant="caption" color="text.secondary">Observed passing runs:</Typography>
          {lifecycle.recovery_builds.map((buildID) => (
            <Link key={buildID} component={RouterLink} to={jobRunPath(jobID, buildID)} sx={overviewTypography.data}>
              {buildID}
            </Link>
          ))}
        </Stack>
      )}
    </Alert>
  ) : null;
  const mobileNotice = staleNotice || lifecycleNotice ? (
    <Stack spacing={1}>
      {staleNotice}
      {lifecycleNotice}
    </Stack>
  ) : undefined;

  const details = (
    <>
      {resolvedEntry && (
        <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}>
          <Chip
            size="small"
            label="Dismissed"
            sx={{
              borderRadius: "4px",
              fontWeight: 650,
              bgcolor: "action.selected",
              color: "text.secondary",
            }}
          />
        </Stack>
      )}

      {resolvedEntry && (
        <Typography color="text.secondary" sx={overviewTypography.description}>
          Dismissed by {resolvedEntry.resolved_by}
          {resolvedEntry.note ? `. ${resolvedEntry.note}` : ""}. It returns to the active view automatically if it recurs.
        </Typography>
      )}

      {staleNotice}
      {lifecycleNotice}

      {pattern.causal_groups && pattern.causal_groups.length > 0 && (
        <BriefingSection label="Causal groups">
          <Stack spacing={1.5}>
            {pattern.causal_groups.map((group) => (
              <Box key={`${group.builds.join("-")}-${group.root_cause}`}>
                <RichText text={group.root_cause} steps fileCtx={patternFileCtx} />
                <Stack
                  direction={{ xs: "column", sm: "row" }}
                  spacing={{ xs: 0.5, sm: 1 }}
                  sx={{ mt: 0.5, alignItems: { sm: "center" } }}
                >
                  <Typography color="text.secondary" sx={overviewTypography.data}>
                    {group.confidence} confidence · Affected {group.builds.length === 1 ? "build" : "builds"}
                  </Typography>
                  <Stack direction="row" spacing={0.75} sx={{ flexWrap: "wrap", rowGap: 0.75 }}>
                    {group.builds.map((buildID) => (
                      <Link
                        key={buildID}
                        component={RouterLink}
                        to={jobID ? jobRunPath(jobID, buildID) : "#"}
                        aria-label={`Open affected build ${buildID}`}
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
                </Stack>
              </Box>
            ))}
          </Stack>
        </BriefingSection>
      )}

      {pattern.unclassified_builds && pattern.unclassified_builds.length > 0 && (
        <BriefingSection label="Unclassified builds">
          <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap", rowGap: 1 }}>
            {pattern.unclassified_builds.map((buildID) => (
              <Link key={buildID} component={RouterLink} to={jobID ? jobRunPath(jobID, buildID) : "#"}>
                {buildID}
              </Link>
            ))}
          </Stack>
        </BriefingSection>
      )}

      {!pattern.causal_groups?.length && pattern.systemic && pattern.shared_root_cause && (
        <BriefingSection label="Root cause">
          <RichText text={pattern.shared_root_cause} steps fileCtx={patternFileCtx} />
        </BriefingSection>
      )}

      {!analysisOnly && pattern.systemic && pattern.suggested_fix && (
        <BriefingSection label="Suggested remediation">
          <RichText text={pattern.suggested_fix} steps fileCtx={patternFileCtx} />
        </BriefingSection>
      )}

      {!analysisOnly && pattern.source_ref && (
        <BriefingSection label="Source grounding">
          <Typography component="code" sx={{ ...overviewTypography.data, overflowWrap: "anywhere" }}>
            {pattern.source_ref}
          </Typography>
        </BriefingSection>
      )}

      {!pattern.causal_groups?.length && pattern.systemic && pattern.shared_builds && pattern.shared_builds.length > 0 && (
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

  const actions = showFixGuidance || chatRef || (!analysisOnly && isCurrent && lifecycleActive && pattern.systemic && pattern.id) ? (
    <Stack spacing={1.25}>
      {jobID && fixGuidanceBuildID && (
        <PatternFixGuidance jobID={jobID} buildID={fixGuidanceBuildID} />
      )}
      {chatRef && (
        <AnalysisChat
          key={`${chatRef.job_id}\u0000${chatRef.pattern_id}\u0000${chatRef.pattern_hash}`}
          analysisRef={chatRef}
          fileCtx={{ builds: buildContexts }}
          fixPatterns={fixPatterns}
          appearance="detail"
        />
      )}
      {!analysisOnly && isCurrent && lifecycleActive && pattern.systemic && pattern.id && (
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
      mobileMetadata={staleNotice ? `Last successful refresh · ${metadata}` : metadata}
      mobileNotice={mobileNotice}
      summary={<RichText text={pattern.summary} steps fileCtx={patternFileCtx} />}
      mobileSynopsis={firstSentence(pattern.shared_root_cause ?? pattern.summary)}
      details={details}
      followUp={analysisOnly ? (
        <PatternRemediation
          groups={pattern.causal_groups ?? []}
          investigations={pattern.remediation_investigations}
          jobID={jobID}
          patternID={pattern.id}
          patternHash={pattern.content_hash}
        />
      ) : undefined}
      actions={actions}
    />
  );
}
