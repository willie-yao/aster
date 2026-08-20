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
  FailureRecurrence,
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
import { BriefingSection } from "./BriefingSection";
import { parseTestDisplayName } from "../lib/detailTitles";
import { FailureActions } from "./FailureActions";
import { useResolved } from "../hooks/useData";
import { AnalysisChat } from "./AnalysisChat";
import { useCapabilities } from "../hooks/useCapabilities";
import { patternChatAvailability, patternChatHasEvidenceBuild } from "../lib/patternChat";
import { patternActionEligibilityHint, patternDismissible, patternDraftable, patternLifecycleActive } from "../lib/actionEligibility";
import { jobRunPath } from "../lib/routes";
import { AnalysisBriefing } from "./AnalysisBriefing";
import { overviewTypography } from "../theme/overview";
import { CausalGroupRemediation } from "./CausalGroupRemediation";
import { CausalGroupFixRouting } from "./CausalGroupFixRouting";
import { PatternFixGuidance } from "./PatternFixGuidance";
import { causalGroupFixTarget, externalCause, patternExternalCause, patternFixGuidanceBuildID } from "../lib/patternFixGuidance";
import { describeRecurrence, recurrenceForBuilds } from "../lib/recurrence";

function firstSentence(value: string): string {
  const match = value.trim().match(/^.*?[.!?](?:\s|$)/u);
  return match?.[0].trim() || value.trim();
}

export function PatternBanner({
  pattern,
  jobID,
  runs = [],
  refreshStatus,
  recurrence,
}: {
  pattern: PatternAnalysis;
  jobID?: string;
  runs?: BuildResult[];
  refreshStatus?: PatternRefreshStatus;
  recurrence?: FailureRecurrence[];
}) {
  const { data: resolved, refetch: refetchResolved } = useResolved();
  const { features } = useCapabilities();
  const analysisOnly = Boolean(pattern.recurrence_classification);
  const causalGroups = pattern.causal_groups ?? [];
  // Fix investigations start from an individual failed test, so the routing is
  // only offered where a chat session could actually run one.
  const fixCapable = Boolean(features.analysis_chat && features.junit_chat_fix);
  const causalFixTargets = causalGroups.map((group) =>
    fixCapable ? causalGroupFixTarget(group, runs) : null,
  );
  // Correlation only ever sees the current window, so a cause it reports as new
  // may have been failing for months.
  const causalRecurrence = causalGroups.map((group) =>
    recurrenceForBuilds(recurrence, group.builds),
  );
  const hasCausalFixTarget = causalFixTargets.some((target) => target !== null);
  // The build joins the label only where two causes would otherwise render the
  // same visible text. Counting the displayed label rather than the canonical
  // name matters: two canonical names can humanize to one display title.
  const fixTargetLabels = causalFixTargets.map((target) =>
    target ? parseTestDisplayName(target.testName).displayName : null,
  );
  const fixTargetLabelCounts = fixTargetLabels.reduce((counts, label) => {
    if (label) counts.set(label, (counts.get(label) ?? 0) + 1);
    return counts;
  }, new Map<string, number>());
  const fixTargetNeedsBuild = fixTargetLabels.map(
    (label) => label !== null && (fixTargetLabelCounts.get(label) ?? 0) > 1,
  );
  const remediationByHash = new Map(
    (pattern.remediation_investigations ?? []).map((summary) => [summary.causal_group_hash, summary]),
  );
  const fixGuidanceBuildID = patternFixGuidanceBuildID(pattern, runs);
  const patternUpstreamCause = patternExternalCause(pattern);
  const showFixGuidance = Boolean(jobID && fixGuidanceBuildID && fixCapable && !hasCausalFixTarget);
  const resolvedEntry = pattern.id ? resolved.resolved[pattern.id] : undefined;
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
  // Dismissal acknowledges the whole pattern, so it is available even where the
  // pattern-level remediation contract is not.
  const dismissible = patternDismissible(pattern, refreshStatus);
  // Drafting follows the remediation contract alone: the two gates are
  // independent, so one must never suppress the other.
  const draftable = patternDraftable(pattern, refreshStatus);
  // A dismissed pattern always offers Restore, even where a fresh dismissal
  // would now be refused: clearing an acknowledgement only un-hides a pattern.
  const showFailureActions = draftable || dismissible || Boolean(resolvedEntry);
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
          <Typography variant="caption" color="textSecondary">Verified passing runs:</Typography>
          {lifecycle.passing_builds.map((buildID) => (
            <Link key={buildID} component={RouterLink} to={jobRunPath(jobID, buildID)} sx={overviewTypography.data}>
              {buildID}
            </Link>
          ))}
        </Stack>
      )}
      {jobID && lifecycle.recovery_builds && lifecycle.recovery_builds.length > 0 && (
        <Stack direction="row" spacing={1} sx={{ mt: 0.75, alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}>
          <Typography variant="caption" color="textSecondary">Observed passing runs:</Typography>
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
        <Typography color="textSecondary" sx={overviewTypography.description}>
          Dismissed by {resolvedEntry.resolved_by}
          {resolvedEntry.note ? `. ${resolvedEntry.note}` : ""}. It returns to the active view automatically if it recurs.
        </Typography>
      )}

      {staleNotice}
      {lifecycleNotice}

      {causalGroups.length > 0 && (
        <BriefingSection label="Causal groups">
          {/* The gap between two causes is deliberately wider than any gap
              inside one, so vertical rhythm expresses the hierarchy on its own. */}
          <Stack spacing={2.5}>
            {causalGroups.map((group, index) => (
              // Keying on group identity ties the remediation component instance
              // to one operation, so a refreshed group never inherits another
              // group's in-flight status or preview.
              <Box
                key={`${group.id ?? ""}:${group.content_hash ?? ""}:${group.builds.join("-")}-${group.root_cause}`}
                sx={{
                  minWidth: 0,
                  border: "1px solid",
                  borderColor: "divider",
                  borderRadius: "4px",
                  bgcolor: "surface.containerLow",
                  overflow: "hidden",
                }}
              >
                <Box
                  sx={{
                    display: "grid",
                    gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "auto minmax(0, 1fr)" },
                    gridTemplateAreas: { xs: '"cause" "confidence"', sm: '"cause confidence"' },
                    alignItems: "center",
                    columnGap: 1.5,
                    rowGap: 0.25,
                    px: 1.5,
                    py: 0.75,
                    bgcolor: "surface.containerHigh",
                    borderBottom: "1px solid",
                    borderColor: "divider",
                    boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
                  }}
                >
                  <Typography
                    component="h4"
                    sx={{ gridArea: "cause", minWidth: 0, ...overviewTypography.subsectionHeading }}
                  >
                    {causalGroups.length > 1 ? `Cause ${index + 1} of ${causalGroups.length}` : "Cause"}
                  </Typography>
                  <Typography
                    component="div"
                    color="textSecondary"
                    sx={{
                      gridArea: "confidence",
                      minWidth: 0,
                      justifySelf: { xs: "start", sm: "end" },
                      textAlign: { xs: "left", sm: "right" },
                      ...overviewTypography.data,
                    }}
                  >
                    {group.confidence} confidence
                    {causalRecurrence[index] &&
                      ` · ${describeRecurrence(causalRecurrence[index])}`}
                  </Typography>
                </Box>
                <Box sx={{ px: 1.5, py: 1.5, minWidth: 0 }}>
                  <RichText text={group.root_cause} steps fileCtx={patternFileCtx} />
                  <Typography
                    component="h5"
                    color="textSecondary"
                    sx={{ mt: 1.5, ...overviewTypography.eyebrow }}
                  >
                    Affected {group.builds.length === 1 ? "build" : "builds"}
                  </Typography>
                  <Stack direction="row" spacing={0.75} sx={{ mt: 0.5, flexWrap: "wrap", rowGap: 0.75 }}>
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
                  {analysisOnly && (
                    <CausalGroupRemediation
                      group={group}
                      investigation={group.content_hash ? remediationByHash.get(group.content_hash) : undefined}
                      jobID={jobID}
                      patternID={pattern.id}
                      patternHash={pattern.content_hash}
                    />
                  )}
                  {fixCapable && (
                    <CausalGroupFixRouting
                      jobID={jobID}
                      target={causalFixTargets[index]}
                      showBuild={fixTargetNeedsBuild[index]}
                      externalCause={externalCause(group.cause_location)}
                    />
                  )}
                </Box>
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

      {causalGroups.length === 0 && pattern.systemic && pattern.shared_root_cause && (
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

      {causalGroups.length === 0 && pattern.systemic && pattern.shared_builds && pattern.shared_builds.length > 0 && (
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

  const actions = showFixGuidance || chatRef || showFailureActions ? (
    <Stack spacing={1.25}>
      {showFixGuidance && jobID && fixGuidanceBuildID && (
        <PatternFixGuidance jobID={jobID} buildID={fixGuidanceBuildID} externalCause={patternUpstreamCause} />
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
      {showFailureActions && pattern.id && (
        <FailureActions
          failureID={pattern.id}
          dismissible={dismissible}
          isResolved={Boolean(resolvedEntry)}
          draftable={draftable}
          eligibilityHint={draftable ? actionEligibility : null}
          onResolvedChange={refetchResolved}
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
      actions={actions}
    />
  );
}
