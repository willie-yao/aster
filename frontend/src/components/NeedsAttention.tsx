import ExpandMore from "@mui/icons-material/ExpandMore";
import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import ReportProblem from "@mui/icons-material/ReportProblem";
import Box from "@mui/material/Box";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { useEffect, useMemo, useState } from "react";
import { Link as RouterLink, useLocation } from "react-router-dom";
import { useResolved } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import {
  attentionGroupNoun,
  attentionGroups,
  attentionSignal,
  countLabel,
  disclosureLabel,
  MAX_OVERVIEW_PATTERNS,
  needsAttentionSummary,
  passRateSummary,
  persistOverviewHistoryState,
  readOverviewHistoryState,
  type AttentionGroup,
} from "../lib/dashboardOverview";
import { jobPath, testPath, testRunPath } from "../lib/routes";
import { patternLifecycleActive } from "../lib/actionEligibility";
import { shortJobName, shortTestName } from "../lib/utils";
import { statusToMuiColor } from "../theme";
import type {
  FlakinessReport,
  JobSummary,
  LowPassRateEntry,
  PatternAnalysis,
  TestFlakiness,
} from "../types/dashboard";
import { Sparkline } from "./Sparkline";
import { overviewLayout, overviewTypography } from "../theme/overview";

const FEATURED_PATTERNS = 3;
const attentionDesktopBreakpoint = "@media (min-width: 1024px)";

interface NeedsAttentionProps {
  report: FlakinessReport | null;
  loading: boolean;
  error: string | null;
  jobsByID: Record<string, JobSummary>;
}

function statusLabel(status: string): string {
  const normalized = status.toLowerCase();
  return normalized
    ? normalized[0].toUpperCase() + normalized.slice(1)
    : status;
}

export function DisclosureButton({
  label,
  open,
  controls,
  onClick,
}: {
  label: string;
  open: boolean;
  controls: string;
  onClick: () => void;
}) {
  return (
    <Box
      component="button"
      type="button"
      onClick={onClick}
      aria-expanded={open}
      aria-controls={controls}
      sx={{
        width: "100%",
        minHeight: 44,
        appearance: "none",
        border: 0,
        borderBlock: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.containerHigh",
        m: 0,
        px: 1,
        py: 0.75,
        cursor: "pointer",
        textAlign: "left",
        font: "inherit",
        color: "inherit",
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        gap: 1,
        "&:hover": {
          bgcolor: "surface.containerHighest",
          color: "text.primary",
        },
        "&:focus-visible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      <Typography
        variant="label"
        component="span"
        color="text.secondary"
        sx={overviewTypography.subsectionHeading}
      >
        {label}
      </Typography>
      <ExpandMore
        aria-hidden="true"
        sx={{
          fontSize: 18,
          color: "text.secondary",
          transition: "transform 140ms ease",
          transform: open ? "rotate(0deg)" : "rotate(-90deg)",
        }}
      />
    </Box>
  );
}

export function FeaturedPatternRow({
  pattern,
  rank,
  prefix,
  stale,
  job,
}: {
  pattern: PatternAnalysis;
  rank: number;
  prefix: string;
  stale: boolean;
  job?: JobSummary;
}) {
  const lead = rank === 1;
  const compactOnMobile = rank > 1;
  const color = job ? statusToMuiColor(job.overall_status) : "default";
  const status = job ? statusLabel(job.overall_status) : "Recurring";
  const signal = attentionSignal(pattern.confidence, stale);
  const jobName = shortJobName(pattern.subject, prefix);

  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr)",
        gridTemplateAreas: '"analysis" "evidence"',
        alignItems: "stretch",
        minHeight: lead ? 156 : 92,
        borderTop: "1px solid",
        borderColor: "divider",
        boxShadow: lead
          ? "inset 3px 0 0 var(--mui-palette-error-main)"
          : "none",
        transition: "background-color 140ms ease",
        "&:hover, &:focus-within": { bgcolor: "surface.containerHigh" },
        [attentionDesktopBreakpoint]: {
          gridTemplateColumns: "minmax(0, 1fr) 300px",
          gridTemplateAreas: '"analysis evidence"',
          minHeight: lead ? 126 : 96,
        },
      }}
    >
      <Link
        component={RouterLink}
        to={jobPath(pattern.job_id ?? "")}
        data-featured-analysis-link
        aria-label={`View analysis for ${jobName}`}
        underline="none"
        sx={{
          gridArea: "analysis",
          minWidth: 0,
          minHeight: 44,
          display: "grid",
          gridTemplateColumns: "40px minmax(0, 1fr)",
          gridTemplateAreas: '"rank subject" ". summary"',
          alignItems: "center",
          columnGap: 1,
          rowGap: 0.5,
          px: 1,
          py: lead ? 1.5 : 1,
          color: "text.primary",
          cursor: "pointer",
          borderRadius: "2px",
          "&:hover .analysis-action": {
            color: "primary.main",
            textDecoration: "underline",
          },
          "&:focus-visible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: -2,
          },
          [attentionDesktopBreakpoint]: {
            gridTemplateColumns: "56px minmax(210px, 1fr) minmax(300px, 2fr)",
            gridTemplateAreas: '"rank subject summary"',
            columnGap: 1.5,
            rowGap: 0,
            px: 1.5,
            py: lead ? 2 : 1.25,
          },
        }}
      >
        <Typography
          variant="stat"
          component="span"
          sx={{
            gridArea: "rank",
            alignSelf: "start",
            color: lead ? "error.main" : "text.secondary",
            fontSize: "18px",
            lineHeight: "24px",
            fontWeight: lead ? 700 : 600,
            [attentionDesktopBreakpoint]: { alignSelf: "center" },
          }}
        >
          {String(rank).padStart(2, "0")}
        </Typography>

        <Box sx={{ gridArea: "subject", minWidth: 0 }}>
          <Typography
            component="span"
            title={jobName}
            sx={{
              display: "block",
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
              ...overviewTypography.jobIdentifier,
            }}
          >
            {jobName}
          </Typography>
          <Typography
            component="span"
            color="text.secondary"
            sx={{ display: "block", ...overviewTypography.description }}
          >
            {job?.category || "Recurring pattern"} ·{" "}
            {job?.branch || "branch unavailable"}
          </Typography>
        </Box>

        <Box sx={{ gridArea: "summary", minWidth: 0 }}>
          <Typography
            variant="body2"
            component="span"
            sx={{
              display: "-webkit-box",
              minWidth: 0,
              overflow: "hidden",
              WebkitBoxOrient: "vertical",
              WebkitLineClamp: compactOnMobile ? 1 : 4,
              ...overviewTypography.primaryBody,
              fontWeight: lead ? 650 : 400,
              ...(lead && {
                "@media (max-width: 599.95px)":
                  overviewTypography.mobileFeaturedBody,
              }),
              [attentionDesktopBreakpoint]: {
                maxInlineSize: "56ch",
                ...overviewTypography.primaryBody,
                WebkitLineClamp: compactOnMobile ? 2 : 3,
              },
            }}
          >
            {pattern.shared_root_cause || pattern.summary}
          </Typography>
          <Typography
            className="analysis-action"
            component="span"
            sx={{
              display: "block",
              mt: 0.5,
              color: "primary.main",
              fontSize: "13px",
              lineHeight: "19px",
              fontWeight: 700,
            }}
          >
            View analysis →
          </Typography>
        </Box>
      </Link>

      <Box
        sx={{
          gridArea: "evidence",
          minWidth: 0,
          pl: 6,
          pr: 1,
          pb: 1,
          [attentionDesktopBreakpoint]: {
            alignSelf: "center",
            justifySelf: "stretch",
            pl: 0,
            pr: 1.5,
            py: 1.25,
            textAlign: "right",
          },
        }}
      >
        <Typography
          variant="caption"
          sx={{
            color: color === "default" ? "text.secondary" : `${color}.main`,
            fontWeight: 700,
            ...overviewTypography.secondaryBody,
          }}
        >
          {status}
        </Typography>
        <Typography
          variant="data"
          color="text.secondary"
          sx={{ display: "block", mt: 0.25, ...overviewTypography.data }}
        >
          {countLabel(pattern.builds_analyzed, "build")}
        </Typography>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ display: "block", mt: 0.25, ...overviewTypography.description }}
        >
          {signal}
        </Typography>
        {job && (
          <Box
            sx={{
              mt: 0.75,
              display: compactOnMobile ? "none" : "flex",
              [attentionDesktopBreakpoint]: {
                display: "flex",
                justifyContent: "flex-end",
              },
            }}
          >
            <Sparkline runs={job.recent_runs} jobID={job.job_id} />
          </Box>
        )}
      </Box>
    </Box>
  );
}

interface AttentionRowProps {
  to: string;
  destinationLabel: string;
  subject: string;
  summary: string;
  detail?: string;
  count?: string;
  signal?: string;
  statusColor?: "success" | "warning" | "error";
  muted?: boolean;
}

export function AttentionRow({
  to,
  destinationLabel,
  subject,
  summary,
  detail,
  count,
  signal,
  statusColor,
  muted = false,
}: AttentionRowProps) {
  return (
    <Box
      component={RouterLink}
      to={to}
      aria-label={destinationLabel}
      sx={{
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr)",
        gridTemplateAreas: '"subject" "summary" "meta"',
        alignItems: "center",
        columnGap: 1.5,
        rowGap: 0.5,
        [attentionDesktopBreakpoint]: {
          gridTemplateColumns:
            "minmax(210px, 1fr) minmax(280px, 2fr) auto 170px",
          gridTemplateAreas: '"subject summary count signal"',
          rowGap: 0,
        },
        minHeight: 48,
        px: 1,
        py: 0.75,
        opacity: muted ? 0.72 : 1,
        borderTop: "1px solid",
        borderColor: "divider",
        borderRadius: "2px",
        color: "text.primary",
        textDecoration: "none",
        cursor: "pointer",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-visible": {
          bgcolor: "surface.containerHigh",
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      <Typography
        component="span"
        title={subject}
        sx={{
          gridArea: "subject",
          minWidth: 0,
          minHeight: 44,
          display: "flex",
          alignItems: "center",
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          ...overviewTypography.jobIdentifier,
          [attentionDesktopBreakpoint]: { minHeight: 0 },
        }}
      >
        {subject}
      </Typography>
      <Box sx={{ gridArea: "summary", minWidth: 0 }}>
        <Typography
          variant="body2"
          component="span"
          title={summary}
          sx={{
            display: "block",
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            ...overviewTypography.secondaryBody,
          }}
        >
          {summary}
        </Typography>
        {detail && (
          <Typography
            variant="caption"
            component="span"
            color="text.secondary"
            title={detail}
            sx={{
              display: "block",
              mt: 0.25,
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
              ...overviewTypography.description,
            }}
          >
            {detail}
          </Typography>
        )}
      </Box>
      <Typography
        variant="data"
        component="span"
        color="text.secondary"
        sx={{
          gridArea: "count",
          display: "none",
          whiteSpace: "nowrap",
          ...overviewTypography.data,
          [attentionDesktopBreakpoint]: { display: "block" },
        }}
      >
        {count}
      </Typography>
      <Typography
        variant="caption"
        component="span"
        color={statusColor ? `${statusColor}.main` : "text.secondary"}
        sx={{
          gridArea: "signal",
          display: "none",
          textAlign: "right",
          whiteSpace: "nowrap",
          ...overviewTypography.description,
          [attentionDesktopBreakpoint]: { display: "block" },
        }}
      >
        {signal}
      </Typography>
      {(count || signal) && (
        <Box
          component="span"
          sx={{
            gridArea: "meta",
            display: "flex",
            alignItems: "center",
            gap: 1.5,
            flexWrap: "wrap",
            [attentionDesktopBreakpoint]: { display: "none" },
          }}
        >
          {count && (
            <Typography
              variant="data"
              component="span"
              color="text.secondary"
              sx={overviewTypography.data}
            >
              {count}
            </Typography>
          )}
          {signal && (
            <Typography
              variant="caption"
              component="span"
              color={statusColor ? `${statusColor}.main` : "text.secondary"}
              sx={overviewTypography.description}
            >
              {signal}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  );
}

export function NeedsAttention({
  report,
  loading,
  error,
  jobsByID,
}: NeedsAttentionProps) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const { data: resolved } = useResolved();
  const location = useLocation();
  const [additionalOpen, setAdditionalOpen] = useState(
    () => readOverviewHistoryState(typeof window === "undefined" ? undefined : window.history.state).additionalOpen,
  );
  const [resolvedOpen, setResolvedOpen] = useState(
    () => readOverviewHistoryState(typeof window === "undefined" ? undefined : window.history.state).resolvedOpen,
  );
  const [expandedGroups, setExpandedGroups] = useState<Record<string, boolean>>(
    () => readOverviewHistoryState(typeof window === "undefined" ? undefined : window.history.state).expandedGroups,
  );

  useEffect(() => {
    persistOverviewHistoryState({ additionalOpen, resolvedOpen, expandedGroups });
  }, [additionalOpen, expandedGroups, location.key, resolvedOpen]);

  const recurring = useMemo<PatternAnalysis[]>(
    () =>
      (report?.recurring_patterns ?? [])
        .filter(
          (pattern) =>
            pattern.job_id &&
            patternLifecycleActive(pattern.lifecycle) &&
            !(pattern.id && resolved.resolved[pattern.id]),
        )
        .slice(0, MAX_OVERVIEW_PATTERNS),
    [report, resolved],
  );

  const resolvedPatterns = useMemo<PatternAnalysis[]>(
    () =>
      (report?.recurring_patterns ?? []).filter(
        (pattern) =>
          pattern.job_id &&
          patternLifecycleActive(pattern.lifecycle) &&
          pattern.id && resolved.resolved[pattern.id],
      ),
    [report, resolved],
  );

  const groups = useMemo<AttentionGroup[]>(
    () => attentionGroups(report, manifest.attention?.low_pass_rate?.threshold),
    [manifest, report],
  );

  const testAlerts = report
    ? groups.reduce((sum, group) => sum + group.items.length, 0)
    : null;
  const summary = needsAttentionSummary(
    report ? recurring.length : null,
    testAlerts,
    loading,
    Boolean(error) || (!loading && !report),
  );
  const featured = recurring.slice(0, FEATURED_PATTERNS);
  const additional = recurring.slice(FEATURED_PATTERNS);
  const hasActiveItems = recurring.length > 0 || groups.length > 0;
  const noActiveAlerts = Boolean(report && !hasActiveItems);

  return (
    <Box
      component="section"
      aria-labelledby="needs-attention-heading"
      sx={{ borderBlock: "1px solid", borderColor: "divider" }}
    >
      <Box
        sx={{
          minHeight: overviewLayout.majorBandMinHeight,
          display: "grid",
          gridTemplateColumns: { xs: "20px minmax(0, 1fr)", sm: "20px auto minmax(0, 1fr)" },
          gridTemplateAreas: { xs: '"icon heading" ". summary"', sm: '"icon heading summary"' },
          alignItems: "center",
          columnGap: 1,
          rowGap: 0.25,
          px: 1.5,
          py: 1,
          bgcolor: "surface.containerHigh",
          boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
        }}
      >
        <ReportProblem color="warning" sx={{ gridArea: "icon", fontSize: 20 }} />
        <Typography
          id="needs-attention-heading"
          variant="headline"
          component="h2"
          tabIndex={-1}
          sx={{
            gridArea: "heading",
            scrollMarginTop: { xs: "128px", lg: "72px" },
            ...overviewTypography.majorHeading,
            "&:focus": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: 2,
            },
          }}
        >
          Needs attention
        </Typography>
        <Typography
          variant="data"
          color="text.secondary"
          sx={{ gridArea: "summary", justifySelf: { sm: "end" }, ...overviewTypography.data }}
        >
          {summary}
        </Typography>
      </Box>

      {loading && (
        <Typography color="text.secondary" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
          Attention data is loading.
        </Typography>
      )}

      {!loading && (error || !report) && (
        <Typography color="text.secondary" sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}>
          Attention data is unavailable.
        </Typography>
      )}

      {noActiveAlerts && (
        <Box sx={{ py: 5, textAlign: "center" }}>
          <CheckCircleOutlined sx={{ fontSize: 28, color: "success.main" }} />
          <Typography
            variant="headline"
            component="h3"
            sx={{ mt: 1, ...overviewTypography.majorHeading }}
          >
            No active test alerts
          </Typography>
          <Typography
            variant="body2"
            color="text.secondary"
            sx={overviewTypography.primaryBody}
          >
            No published test-level or recurring-pattern alerts need attention.
          </Typography>
        </Box>
      )}

      {report && hasActiveItems && (
        <>
          {featured.map((pattern, index) => {
            const refreshStatus = report.pattern_refresh?.jobs?.[pattern.job_id ?? ""];
            return (
              <FeaturedPatternRow
                key={pattern.id ?? pattern.job_id ?? pattern.subject}
                pattern={pattern}
                rank={index + 1}
                prefix={filePrefix}
                stale={Boolean(refreshStatus && refreshStatus.state !== "current")}
                job={pattern.job_id ? jobsByID[pattern.job_id] : undefined}
              />
            );
          })}

          {additional.length > 0 && (
            <Box component="section">
              <DisclosureButton
                label={disclosureLabel(
                  additionalOpen,
                  additional.length,
                  "additional recurring pattern",
                  "additional recurring patterns",
                )}
                open={additionalOpen}
                controls="additional-recurring-patterns"
                onClick={() => setAdditionalOpen((open) => !open)}
              />
              <Collapse
                id="additional-recurring-patterns"
                in={additionalOpen}
                timeout="auto"
                unmountOnExit
              >
                {additional.map((pattern) => {
                  const refreshStatus = report.pattern_refresh?.jobs?.[pattern.job_id ?? ""];
                  const stale = Boolean(refreshStatus && refreshStatus.state !== "current");
                  const subject = shortJobName(pattern.subject, filePrefix);
                  return (
                    <AttentionRow
                      key={pattern.id ?? pattern.job_id ?? pattern.subject}
                      to={jobPath(pattern.job_id ?? "")}
                      destinationLabel={`View analysis for ${subject}`}
                      subject={subject}
                      summary={pattern.shared_root_cause || pattern.summary}
                      count={countLabel(pattern.builds_analyzed, "build")}
                      signal={attentionSignal(pattern.confidence, stale)}
                    />
                  );
                })}
              </Collapse>
            </Box>
          )}

          {groups.map((group) => {
            const progressive = group.kind !== "recent";
            const initialCount = group.kind === "recent"
              ? group.items.length
              : group.kind === "persistent" ? 1 : 3;
            const open = expandedGroups[group.label] ?? false;
            const initialItems = progressive ? group.items.slice(0, initialCount) : group.items;
            const additionalItems = progressive ? group.items.slice(initialCount) : [];
            const controls = `attention-group-${group.label.toLowerCase().replace(/\s+/g, "-")}`;
            const itemNoun = attentionGroupNoun(group.kind);
            const renderItem = (item: TestFlakiness) => {
              // The pass-rate group can select a test that has already
              // recovered, so it reports the measured rate and the test's own
              // classification instead of borrowing the failing styling.
              const lowPassRate = group.kind === "lowPassRate"
                ? (item as LowPassRateEntry)
                : undefined;
              const failing = item.classification !== "flaky";
              const statusColor = lowPassRate
                ? item.classification === "persistent" ? "error" : "warning"
                : failing ? "error" : "warning";
              const consecutive = item.consecutive_failures > 0
                ? `${item.consecutive_failures} consecutive ${item.consecutive_failures === 1 ? "failure" : "failures"}`
                : undefined;
              const subject = shortJobName(item.job_name, filePrefix);
              const testName = shortTestName(item.test_name);
              const destination = item.last_failure
                ? testRunPath(item.job_id, item.test_name, item.last_failure.build_id)
                : testPath(item.job_id, item.test_name);
              return (
                <AttentionRow
                  key={`${item.job_id}/${item.test_name}`}
                  to={destination}
                  destinationLabel={`${item.last_failure ? "Open latest test run" : "Open test"} for ${testName} in ${subject}`}
                  subject={subject}
                  summary={testName}
                  detail={item.last_failure?.failure_message}
                  count={lowPassRate ? passRateSummary(lowPassRate) : consecutive}
                  signal={lowPassRate ? statusLabel(item.classification) : failing ? "Failing" : "Flaky"}
                  statusColor={statusColor}
                />
              );
            };
            return (
              <Box key={group.label} component="section" aria-label={group.label} sx={{ mt: 1 }}>
                <Box
                  sx={{
                    minHeight: overviewLayout.subsectionBandMinHeight,
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 1,
                    px: 1.25,
                    py: 0.75,
                    bgcolor: "surface.containerHigh",
                    borderBlock: "1px solid",
                    borderColor: "divider",
                  }}
                >
                  <Typography
                    variant="label"
                    component="h3"
                    color="text.secondary"
                    sx={overviewTypography.subsectionHeading}
                  >
                    {group.label}
                  </Typography>
                  <Typography variant="data" color="text.secondary" sx={overviewTypography.data}>
                    {group.items.length}
                  </Typography>
                </Box>
                <Box>{initialItems.map(renderItem)}</Box>
                {progressive && additionalItems.length > 0 && (
                  <>
                    <DisclosureButton
                      label={disclosureLabel(open, additionalItems.length, itemNoun[0], itemNoun[1])}
                      open={open}
                      controls={controls}
                      onClick={() =>
                        setExpandedGroups((current) => ({ ...current, [group.label]: !open }))
                      }
                    />
                    <Collapse id={controls} in={open} timeout="auto" unmountOnExit>
                      {additionalItems.map(renderItem)}
                    </Collapse>
                  </>
                )}
              </Box>
            );
          })}

        </>
      )}

      {report && resolvedPatterns.length > 0 && (
        <Box component="section">
          <DisclosureButton
            label={disclosureLabel(
              resolvedOpen,
              resolvedPatterns.length,
              "dismissed pattern",
              "dismissed patterns",
            )}
            open={resolvedOpen}
            controls="dismissed-patterns"
            onClick={() => setResolvedOpen((open) => !open)}
          />
          <Collapse id="dismissed-patterns" in={resolvedOpen} timeout="auto" unmountOnExit>
            {resolvedPatterns.map((pattern) => {
              const entry = pattern.id ? resolved.resolved[pattern.id] : undefined;
              const subject = shortJobName(pattern.subject, filePrefix);
              return (
                <AttentionRow
                  key={pattern.id ?? pattern.job_id ?? pattern.subject}
                  to={jobPath(pattern.job_id ?? "")}
                  destinationLabel={`View dismissed analysis for ${subject}`}
                  subject={subject}
                  summary={pattern.shared_root_cause || pattern.summary}
                  detail={entry?.note}
                  signal="Dismissed"
                  muted
                />
              );
            })}
          </Collapse>
        </Box>
      )}
    </Box>
  );

}
