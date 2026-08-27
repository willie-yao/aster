import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { AutoAwesome, ExpandMore } from "@mui/icons-material";
import { useEffect, useState } from "react";
import type {
  BuildResult,
  FailureRecurrence,
  PatternAnalysis,
  PatternCausalGroup,
  PatternRefreshStatus,
} from "../types/dashboard";
import type { AnalysisChatReference, CauseAnalysisChatReference } from "../types/analysisChat";
import {
  fileSortKey,
  fileToUrl,
  meetsConfidenceFloor,
  timeAgo,
  type FileToUrlContext,
} from "../lib/utils";
import { RichText } from "./RichText";
import { BriefingSection } from "./BriefingSection";
import { parseTestDisplayName } from "../lib/detailTitles";
import { FailureActions } from "./FailureActions";
import { useResolved } from "../hooks/useData";
import { AnalysisChat } from "./AnalysisChat";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import { patternChatAvailability, patternChatHasEvidenceBuild } from "../lib/patternChat";
import { lookupPreparedAnalysisChatFindings, applyPreparedFindingResolution, type PreparedFindingResult } from "../lib/analysisChat";
import { patternActionEligibilityHint, patternActionRefreshBlocked, patternResolvable, patternDraftable, patternLifecycleActive, causeResolvable, patternResolutionCovered } from "../lib/actionEligibility";
import { jobRunPath } from "../lib/routes";
import { buildsAnalyzedLabel, patternCountOutdated } from "../lib/dashboardOverview";
import { AnalysisBriefing } from "./AnalysisBriefing";
import { overviewTypography, touchTargetSx } from "../theme/overview";
import { CausalGroupNextStep } from "./CausalGroupNextStep";
import { CausalGroupFixButton } from "./CausalGroupFixRouting";
import { CauseResolution } from "./CauseResolution";
import { causeResolutionAvailable } from "../lib/resolution";
import { PatternFixGuidance } from "./PatternFixGuidance";
import { causalGroupEvidencePresent, causalGroupFixTarget, externalCause, patternExternalCause, patternFixGuidanceBuildID } from "../lib/patternFixGuidance";
import { describeRecurrence, recurrenceForBuilds } from "../lib/recurrence";
import { accentLabelSx } from "../theme";

function firstSentence(value: string): string {
  const match = value.trim().match(/^.*?[.!?](?:\s|$)/u);
  return match?.[0].trim() || value.trim();
}

// The summary rail links to a cause card, so both sides derive the anchor here
// rather than restating the key and drifting apart.
function causeCardID(pattern: PatternAnalysis, causeKey: string): string {
  return `cause-card-${pattern.id ?? "pattern"}-${causeKey}`;
}

function causeKeyFor(group: PatternCausalGroup, index: number): string {
  return group.signature ?? group.id ?? String(index);
}

// Identity of one cause within a prepared-finding lookup result, so the answer
// survives the causal groups being reordered between request and response.
function preparedCauseKey(ref: CauseAnalysisChatReference): string {
  return `${ref.causal_group_id}\u0000${ref.causal_group_hash}`;
}

const noPreparedCauses: Record<string, boolean> = {};

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
  const { status: authStatus } = useAuth();
  // Explicit per-cause expansion overrides. Absent entries fall back to the
  // resolution state, so a cause folds when resolved without pinning it there.
  const [expandedCauses, setExpandedCauses] = useState<Record<string, boolean>>({});
  const analysisOnly = Boolean(pattern.recurrence_classification);
  const causalGroups = pattern.causal_groups ?? [];
  // Fix proposals start from an individual failed test, so the routing is
  // only offered where a chat session could actually run one.
  const fixCapable = Boolean(features.analysis_chat && features.junit_chat_fix);
  const causalFixTargets = causalGroups.map((group) =>
    fixCapable ? causalGroupFixTarget(group, runs) : null,
  );
  const causalEvidencePresent = causalGroups.map((group) => causalGroupEvidencePresent(group, runs));
  const availableBuildIDs = new Set(runs.map((run) => run.build_id));
  const causeChatRefs = causalGroups.map((group) =>
    features.analysis_chat && jobID && pattern.id && pattern.content_hash && group.id && group.content_hash &&
      group.builds.length > 0 && group.builds.every((buildID) => availableBuildIDs.has(buildID))
      ? {
          scope: "cause" as const,
          job_id: jobID,
          pattern_id: pattern.id,
          pattern_hash: pattern.content_hash,
          causal_group_id: group.id,
          causal_group_hash: group.content_hash,
        }
      : null,
  );
  // One read-only batch for every cause on the page. Asking per card through
  // the create path would open shared sessions for causes nobody looked at.
  const preparedLookupKey = JSON.stringify(causeChatRefs.filter((ref) => ref !== null));
  const [preparedResult, setPreparedResult] = useState<PreparedFindingResult>(
    { key: "", causes: noPreparedCauses },
  );
  useEffect(() => {
    if (!features.analysis_chat || authStatus !== "authenticated") return;
    const refs = JSON.parse(preparedLookupKey) as CauseAnalysisChatReference[];
    if (refs.length === 0) return;
    const controller = new AbortController();
    void (async () => {
      try {
        const prepared = await lookupPreparedAnalysisChatFindings(refs, controller.signal);
        if (controller.signal.aborted) return;
        setPreparedResult({
          key: preparedLookupKey,
          causes: Object.fromEntries(refs.map((ref, index) => [preparedCauseKey(ref), prepared[index]])),
        });
      } catch {
        // The marker is advisory. A failed lookup leaves the control unmarked
        // rather than surfacing an error the operator cannot act on.
      }
    })();
    return () => controller.abort();
  }, [authStatus, features.analysis_chat, preparedLookupKey]);
  const preparedCauses = preparedResult.key === preparedLookupKey ? preparedResult.causes : noPreparedCauses;
  const causePreparedKeys = causeChatRefs.map((ref) => (ref ? preparedCauseKey(ref) : ""));
  const recordPrepared = (cause: string, ready: boolean) => {
    setPreparedResult((current) => applyPreparedFindingResolution(current, preparedLookupKey, cause, ready));
  };
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
    causalGroups.length === 0 && chatAvailability === "ready" && pattern.id && pattern.content_hash && jobID
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
  const lifecycle = pattern.lifecycle;
  const lifecycleActive = patternLifecycleActive(lifecycle);
  const causeResolutions = causalGroups.map((group) =>
    group.signature ? resolved.causes[group.signature] : undefined,
  );
  const causeResolvableFlags = causalGroups.map((group) =>
    causeResolvable(pattern, group, refreshStatus),
  );
  // The bar draws a rule above itself, so it renders only where at least one of
  // the two controls will. Both gates are asked the same way their components
  // ask them, so the rule can never appear above an empty row.
  const causeActionsPresent = causalGroups.map((group, index) =>
    Boolean(
      (fixCapable && jobID && causalFixTargets[index]) ||
      causeResolutionAvailable({
        actionsEnabled: Boolean(features.actions),
        authenticated: authStatus === "authenticated",
        signature: group.signature,
        resolvable: causeResolvableFlags[index],
        resolved: Boolean(causeResolutions[index]),
      }),
    ),
  );
  // Per-cause resolution replaces the pattern-level control wherever it covers
  // every cause, so a maintainer is never offered two acknowledgements with
  // different blast radii for the same pattern.
  const causeResolutionCovers = patternResolutionCovered(pattern, refreshStatus);
  const canResolve = patternResolvable(pattern, refreshStatus) && !causeResolutionCovers;
  // Drafting follows the remediation contract alone: the two gates are
  // independent, so one must never suppress the other.
  const draftable = patternDraftable(pattern, refreshStatus);
  // A resolved pattern always offers Reopen, even where a fresh resolution
  // would now be refused: clearing an acknowledgement only un-hides a pattern.
  // An anonymous viewer keeps the block wherever per-cause controls would
  // render for a signed-in one, because it carries the sign-in prompt. That
  // covers causes that are already resolved as well as freshly resolvable ones,
  // since a resolved cause still offers Reopen after it stops qualifying.
  const causeControlsPresent = causeResolutionCovers || causeResolutions.some(Boolean);
  const showFailureActions =
    draftable || canResolve || Boolean(resolvedEntry) ||
    (authStatus === "anonymous" && causeControlsPresent);
  const actionEligibility = patternActionEligibilityHint(
    pattern.remediation_targets,
    lifecycle,
    pattern.systemic,
    refreshStatus,
  );
  const fixPatterns =
    !analysisOnly &&
    // The pattern-scope fix confirms through the actions service, which turns on
    // readable evidence rather than a fresh correlation, so this mirrors that
    // instead of requiring the correlation to have refreshed.
    !patternActionRefreshBlocked(refreshStatus) &&
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
  const metadata = `${buildsAnalyzedLabel(pattern, refreshStatus)} · ${pattern.confidence} confidence`;
  const staleNotice = refreshStatus && refreshStatus.state !== "current" ? (
    <Alert severity="warning" role="status" variant="outlined" sx={{ borderRadius: "4px" }}>
      Pattern from the last successful refresh at {refreshStatus.last_successful_at ?? "an earlier time"}.
      Current refresh status: {refreshStatus.failure_category ?? refreshStatus.state}.
      {patternCountOutdated(pattern, refreshStatus) &&
        " Some of the builds it correlated have aged out of the analysis window, so its build count covers a wider window than the current one."}
    </Alert>
  ) : null;
  const lifecycleNotice = lifecycle && !lifecycleActive ? (
    <Alert
      severity={lifecycle.state === "verified_fixed" ? "success" : "info"}
      role="status"
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
            label="Resolved"
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
          Resolved by {resolvedEntry.resolved_by}
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
            {causalGroups.map((group, index) => {
              // Only a resolved cause folds away, and it is keyed by the same
              // signature its resolution is recorded under. The default follows
              // resolution state, so resolving folds the cause and reopening
              // unfolds it, while an explicit toggle overrides both.
              // The override is scoped to one specific resolution event, so a
              // cause that is reopened and then resolved again folds itself
              // away instead of inheriting the expansion from last time.
              const causeKey = causeKeyFor(group, index);
              const collapsible = Boolean(causeResolutions[index]);
              const overrideKey = `${causeKey}:${causeResolutions[index]?.resolved_at ?? ""}`;
              // Only a collapsible cause consults the override. An unresolved
              // cause is always open, so a stale override left behind by
              // collapsing a cause and then reopening it cannot strand the body
              // hidden with no toggle left to show it.
              const expanded = collapsible ? expandedCauses[overrideKey] ?? false : true;
              const bodyID = `cause-body-${pattern.id ?? "pattern"}-${causeKey}`;
              const cardID = causeCardID(pattern, causeKey);
              return (
              // Keying on group identity ties the remediation component instance
              // to one operation, so a refreshed group never inherits another
              // group's in-flight status or preview.
              <Box
                key={`${group.id ?? ""}:${group.content_hash ?? ""}:${group.builds.join("-")}-${group.root_cause}`}
                id={cardID}
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
                    position: "relative",
                    display: "grid",
                    gridTemplateColumns: {
                      xs: "minmax(0, 1fr) auto",
                      sm: "auto minmax(0, 1fr) auto",
                    },
                    gridTemplateAreas: {
                      xs: '"cause toggle" "confidence confidence"',
                      sm: '"cause confidence toggle"',
                    },
                    alignItems: "center",
                    columnGap: 1.5,
                    rowGap: 0.25,
                    px: 1.5,
                    py: 0.75,
                    bgcolor: "surface.containerHigh",
                    borderBottom: expanded ? "1px solid" : 0,
                    borderColor: "divider",
                    boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
                  }}
                >
                  <Stack
                    direction="row"
                    spacing={1}
                    sx={{ gridArea: "cause", minWidth: 0, alignItems: "center", flexWrap: "wrap", rowGap: 0.5 }}
                  >
                    <Typography component="h4" sx={{ m: 0, minWidth: 0, ...overviewTypography.subsectionHeading }}>
                      {collapsible ? (
                        // The heading wraps the toggle rather than sitting
                        // inside it: a heading is not valid phrasing content
                        // within a button, and assistive technology exposes
                        // that nesting inconsistently. The ::after overlay is
                        // what keeps the whole header band clickable.
                        <ButtonBase
                          disableRipple
                          aria-expanded={expanded}
                          aria-controls={bodyID}
                          onClick={() =>
                            setExpandedCauses((current) => ({ ...current, [overrideKey]: !expanded }))
                          }
                          sx={{
                            font: "inherit",
                            color: "inherit",
                            textAlign: "left",
                            // ButtonBase is position: relative by default, which
                            // would anchor the overlay to the label instead of
                            // the header band it is meant to cover.
                            position: "static",
                            "&::after": { content: '""', position: "absolute", inset: 0 },
                            "&:hover::after": { bgcolor: "action.hover" },
                            "&:focus-visible::after": {
                              outline: "2px solid",
                              outlineColor: "primary.main",
                              outlineOffset: -2,
                            },
                          }}
                        >
                          {causalGroups.length > 1 ? `Cause ${index + 1} of ${causalGroups.length}` : "Cause"}
                        </ButtonBase>
                      ) : (
                        causalGroups.length > 1 ? `Cause ${index + 1} of ${causalGroups.length}` : "Cause"
                      )}
                    </Typography>
                    {causeResolutions[index] && (
                      <Chip
                        size="small"
                        label="Resolved"
                        sx={{
                          borderRadius: "4px",
                          fontWeight: 650,
                          bgcolor: "action.selected",
                          color: "text.secondary",
                        }}
                      />
                    )}
                  </Stack>
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
                  {collapsible && (
                    <ExpandMore
                      aria-hidden
                      sx={{
                        gridArea: "toggle",
                        fontSize: 20,
                        color: "text.secondary",
                        transform: expanded ? "rotate(180deg)" : "none",
                        transition: "transform 150ms",
                      }}
                    />
                  )}
                </Box>
                <Box id={bodyID}>
                  <Collapse in={expanded} timeout="auto" unmountOnExit>
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
                        sx={(theme) => ({
                          ...touchTargetSx,
                          display: "inline-flex",
                          alignItems: "center",
                          px: 0.75,
                          borderRadius: "4px",
                          bgcolor: "action.selected",
                          ...accentLabelSx(theme, "primary"),
                          ...overviewTypography.data,
                          "&:hover": { bgcolor: "surface.containerHigh" },
                          "&:focus-visible": {
                            outline: "2px solid",
                            outlineColor: "primary.main",
                            outlineOffset: 2,
                          },
                        })}
                      >
                        {buildID}
                      </Link>
                    ))}
                  </Stack>
                  <CausalGroupNextStep
                    group={group}
                    jobID={jobID}
                    fileCtx={patternFileCtx}
                    chat={
                      causeChatRefs[index]
                        ? {
                            ref: causeChatRefs[index],
                            fileCtx: {
                              builds: Object.fromEntries(
                                group.builds.flatMap((buildID) =>
                                  buildContexts[buildID] ? [[buildID, buildContexts[buildID]]] : [],
                                ),
                              ),
                              fileLinks: pattern.file_links,
                            },
                            preparedFinding: preparedCauses[causePreparedKeys[index]],
                            onPreparedResolved: (ready: boolean) => recordPrepared(causePreparedKeys[index], ready),
                          }
                        : undefined
                    }
                    routing={
                      fixCapable
                        ? {
                            target: causalFixTargets[index],
                            externalCause: externalCause(group.cause_location),
                            // A target exists only where the cause's build is
                            // still readable, so the offer turns on the
                            // pattern's lifecycle rather than on whether the
                            // correlation refreshed: a recovered or
                            // verified-fixed cause is worth viewing but not
                            // worth fixing.
                            stale: !lifecycleActive,
                            evidencePresent: causalEvidencePresent[index],
                          }
                        : undefined
                    }
                  />
                  {causeActionsPresent[index] && (
                    // Both of a cause's actions share one bar, so the card ends
                    // with a single place to act rather than one control buried
                    // mid-body and another below a rule. It wraps because the
                    // resolution error takes its own line.
                    <Stack
                      direction={{ xs: "column", sm: "row" }}
                      spacing={1}
                      sx={{
                        mt: 1.5,
                        pt: 1.5,
                        borderTop: "1px solid",
                        borderColor: "divider",
                        // Narrow rows give each control its own line: the route's
                        // build suffix and the resolution control both refuse to
                        // shrink, so side by side they would overlap.
                        alignItems: { xs: "stretch", sm: "center" },
                        // The route reads from the start of the bar and the
                        // action anchors its end, so a wide row has no gap
                        // sitting between the two.
                        justifyContent: "space-between",
                        flexWrap: "wrap",
                        rowGap: 1,
                        minWidth: 0,
                      }}
                    >
                      {fixCapable && (
                        <CausalGroupFixButton
                          jobID={jobID}
                          target={causalFixTargets[index]}
                          showBuild={fixTargetNeedsBuild[index]}
                          stale={!lifecycleActive}
                        />
                      )}
                      <CauseResolution
                        signature={group.signature}
                        resolvedEntry={causeResolutions[index]}
                        resolvable={causeResolvableFlags[index]}
                        onResolvedChange={refetchResolved}
                      />
                    </Stack>
                  )}
                </Box>
                  </Collapse>
                </Box>
              </Box>
              );
            })}
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
                sx={(theme) => ({
                  ...touchTargetSx,
                  display: "inline-flex",
                  alignItems: "center",
                  px: 0.75,
                  borderRadius: "4px",
                  bgcolor: "action.selected",
                  ...accentLabelSx(theme, "primary"),
                  ...overviewTypography.data,
                  "&:hover": { bgcolor: "surface.containerHigh" },
                  "&:focus-visible": {
                    outline: "2px solid",
                    outlineColor: "primary.main",
                    outlineOffset: 2,
                  },
                })}
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
        <Alert role="status" severity="info" variant="outlined" sx={{ borderRadius: "4px" }}>
          Recurring-pattern chat is unavailable because this dashboard data predates content hashing.
          Refresh the dashboard data to enable it.
        </Alert>
      )}
    </>
  );

  const actions = showFixGuidance || chatRef || showFailureActions ? (
    <Stack spacing={1.25}>
      {showFixGuidance && jobID && fixGuidanceBuildID && (
        <PatternFixGuidance jobID={jobID} buildID={fixGuidanceBuildID} externalCause={patternUpstreamCause} chatAvailable={Boolean(chatRef)} />
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
          canResolve={canResolve}
          isResolved={Boolean(resolvedEntry)}
          draftable={draftable}
          eligibilityHint={draftable ? actionEligibility : null}
          onResolvedChange={refetchResolved}
          appearance="detail"
        />
      )}
    </Stack>
  ) : undefined;

  const causeCount = causalGroups.length > 0 ? String(causalGroups.length) : "None isolated";

  // The band beside the title already carries builds and confidence, so this
  // rail answers what it does not: when the analysis ran, and a way into the
  // causes without scrolling for them.
  const summaryFacts = (
    <Box
      sx={{
        display: "flex",
        flexDirection: "column",
        gap: 1.25,
        // The rule only reads as a rail when the stack sits beside the prose.
        "@media (min-width: 1100px)": { borderLeft: 1, borderColor: "divider", pl: 2.5 },
      }}
    >
      {[
        { label: "Analysed", value: timeAgo(pattern.generated_at) },
        {
          label: "Distinct causes",
          value: causalGroups.length > 0 ? (
            <Link
              href={`#${causeCardID(pattern, causeKeyFor(causalGroups[0], 0))}`}
              underline="hover"
              sx={{ color: "primary.main" }}
            >
              {causeCount}
            </Link>
          ) : causeCount,
        },
        { label: "Confidence", value: pattern.confidence },
      ].map((fact) => (
        <Box key={fact.label}>
          <Typography variant="caption" color="textSecondary" sx={{ display: "block", fontWeight: 700 }}>
            {fact.label}
          </Typography>
          <Typography component="div" sx={{ ...overviewTypography.data, color: "text.primary" }}>
            {fact.value}
          </Typography>
        </Box>
      ))}
    </Box>
  );

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
      summaryAside={summaryFacts}
      mobileSynopsis={firstSentence(pattern.shared_root_cause ?? pattern.summary)}
      details={details}
      actions={actions}
    />
  );
}
