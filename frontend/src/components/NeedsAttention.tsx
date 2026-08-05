import ExpandMore from "@mui/icons-material/ExpandMore";
import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import ReportProblem from "@mui/icons-material/ReportProblem";
import Box from "@mui/material/Box";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { useMemo, useState } from "react";
import { Link as RouterLink } from "react-router-dom";
import { useResolved } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { attentionSignal, countLabel, MAX_OVERVIEW_PATTERNS } from "../lib/dashboardOverview";
import { jobPath, testPath, testRunPath } from "../lib/routes";
import { shortJobName, shortTestName } from "../lib/utils";
import { statusToMuiColor } from "../theme";
import type { FlakinessReport, JobSummary, PatternAnalysis, TestFlakiness } from "../types/dashboard";
import { Sparkline } from "./Sparkline";
import { overviewLayout, overviewTypography } from "../theme/overview";

const MAX_ITEMS = 10;
const FEATURED_PATTERNS = 3;
const attentionDesktopBreakpoint = "@media (min-width: 1024px)";

interface ItemGroup {
  label: string;
  items: TestFlakiness[];
}

interface NeedsAttentionProps {
  report: FlakinessReport | null;
  loading: boolean;
  error: string | null;
  jobsByID: Record<string, JobSummary>;
}

function statusLabel(status: string): string {
  const normalized = status.toLowerCase();
  return normalized ? normalized[0].toUpperCase() + normalized.slice(1) : status;
}

function DisclosureButton({
  label,
  count,
  open,
  controls,
  onClick,
}: {
  label: string;
  count: number;
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
        background: "transparent",
        cursor: "pointer",
        textAlign: "left",
        font: "inherit",
        color: "inherit",
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        gap: 1,
        "&:hover": { bgcolor: "surface.containerHigh" },
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
        {label} ({count})
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

function FeaturedPatternRow({
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

  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: "40px minmax(0, 1fr)",
        gridTemplateAreas: '"rank subject" ". summary" ". evidence"',
        alignItems: "center",
        columnGap: 1,
        rowGap: 0.5,
        minHeight: lead ? 156 : 92,
        px: 1,
        py: lead ? 1.5 : 1,
        [attentionDesktopBreakpoint]: {
          gridTemplateColumns: "56px minmax(210px, 1fr) minmax(300px, 2fr) 190px",
          gridTemplateAreas: '"rank subject summary evidence"',
          columnGap: 1.5,
          rowGap: 0,
          minHeight: lead ? 126 : 96,
          px: 1.5,
          py: lead ? 2 : 1.25,
        },
        borderTop: "1px solid",
        borderColor: lead ? "error.main" : "divider",
        boxShadow: lead ? "inset 3px 0 0 var(--mui-palette-error-main)" : "none",
      }}
    >
      <Typography
        variant="stat"
        component="span"
        sx={{ gridArea: "rank", alignSelf: "start", color: lead ? "error.main" : "text.secondary", fontSize: lead ? "1.5rem" : "1rem", [attentionDesktopBreakpoint]: { alignSelf: "center" } }}
      >
        {String(rank).padStart(2, "0")}
      </Typography>

      <Box sx={{ gridArea: "subject", minWidth: 0 }}>
        <Link
          component={RouterLink}
          to={jobPath(pattern.job_id ?? "")}
          underline="none"
          sx={{
            minHeight: 44,
            display: "flex",
            alignItems: "center",
            color: "text.primary",
            ...overviewTypography.jobIdentifier,
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            "&:hover": { color: "primary.main", textDecoration: "underline" },
            "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 1 },
            [attentionDesktopBreakpoint]: { minHeight: 0 },
          }}
        >
          {shortJobName(pattern.subject, prefix)}
        </Link>
        <Typography variant="caption" color="text.secondary" sx={overviewTypography.description}>
          {job?.category || "Recurring pattern"} · {job?.branch || "branch unavailable"}
        </Typography>
      </Box>

      <Typography
        variant="body2"
        sx={{
          gridArea: "summary",
          minWidth: 0,
          ...overviewTypography.primaryBody,
          fontWeight: lead ? 650 : 400,
          display: "-webkit-box",
          WebkitBoxOrient: "vertical",
          WebkitLineClamp: compactOnMobile ? 1 : 4,
          overflow: "hidden",
          ...(lead && {
            "@media (max-width: 599.95px)": overviewTypography.mobileFeaturedBody,
          }),
          [attentionDesktopBreakpoint]: {
            ...overviewTypography.primaryBody,
            WebkitLineClamp: compactOnMobile ? 2 : 3,
          },
        }}
      >
        {pattern.shared_root_cause || pattern.summary}
      </Typography>

      <Box sx={{ gridArea: "evidence", minWidth: 0, [attentionDesktopBreakpoint]: { justifySelf: "end", textAlign: "right" } }}>
        <Typography
          variant="caption"
          sx={{ color: color === "default" ? "text.secondary" : `${color}.main`, fontWeight: 700, ...overviewTypography.secondaryBody }}
        >
          {status}
        </Typography>
        <Typography variant="data" color="text.secondary" sx={{ display: "block", mt: 0.25, ...overviewTypography.data }}>
          {countLabel(pattern.builds_analyzed, "build")}
        </Typography>
        <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.25, ...overviewTypography.description }}>
          {signal}
        </Typography>
        {job && (
          <Box
            sx={{
              mt: 0.75,
              display: compactOnMobile ? "none" : "flex",
              [attentionDesktopBreakpoint]: { display: "flex", justifyContent: "flex-end" },
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
  subject: string;
  summary: string;
  detail?: string;
  count?: string;
  signal?: string;
  statusColor?: "success" | "warning" | "error";
  muted?: boolean;
}

function AttentionRow({
  to,
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
      sx={{
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr)",
        gridTemplateAreas: '"subject" "summary" "meta"',
        alignItems: "center",
        columnGap: 1.5,
        rowGap: 0.5,
        [attentionDesktopBreakpoint]: {
          gridTemplateColumns: "minmax(210px, 1fr) minmax(280px, 2fr) auto 170px",
          gridTemplateAreas: '"subject summary count signal"',
          rowGap: 0,
        },
        minHeight: 48,
        px: 1,
        py: 0.75,
        opacity: muted ? 0.72 : 1,
        borderTop: "1px solid",
        borderColor: "divider",
        "&:hover": { bgcolor: "surface.containerHigh" },
      }}
    >
      <Link
        component={RouterLink}
        to={to}
        underline="none"
        title={subject}
        sx={{
          gridArea: "subject",
          minWidth: 0,
          minHeight: { xs: 44, md: 0 },
          display: "flex",
          alignItems: "center",
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          color: "text.primary",
          ...overviewTypography.jobIdentifier,
          "&:hover": { color: "primary.main", textDecoration: "underline" },
          "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 1 },
        }}
      >
        {subject}
      </Link>
      <Box sx={{ gridArea: "summary", minWidth: 0 }}>
        <Typography
          variant="body2"
          title={summary}
          sx={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", ...overviewTypography.secondaryBody }}
        >
          {summary}
        </Typography>
        {detail && (
          <Typography
            variant="caption"
            color="text.secondary"
            title={detail}
            sx={{ display: "block", mt: 0.25, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", ...overviewTypography.description }}
          >
            {detail}
          </Typography>
        )}
      </Box>
      <Typography variant="data" color="text.secondary" sx={{ gridArea: "count", display: "none", whiteSpace: "nowrap", ...overviewTypography.data, [attentionDesktopBreakpoint]: { display: "block" } }}>
        {count}
      </Typography>
      <Typography variant="caption" color={statusColor ? `${statusColor}.main` : "text.secondary"} sx={{ gridArea: "signal", display: "none", textAlign: "right", whiteSpace: "nowrap", ...overviewTypography.description, [attentionDesktopBreakpoint]: { display: "block" } }}>
        {signal}
      </Typography>
      {(count || signal) && (
        <Box sx={{ gridArea: "meta", display: "flex", alignItems: "center", gap: 1.5, flexWrap: "wrap", [attentionDesktopBreakpoint]: { display: "none" } }}>
          {count && <Typography variant="data" color="text.secondary" sx={overviewTypography.data}>{count}</Typography>}
          {signal && <Typography variant="caption" color={statusColor ? `${statusColor}.main` : "text.secondary"} sx={overviewTypography.description}>{signal}</Typography>}
        </Box>
      )}
    </Box>
  );
}

export function NeedsAttention({ report, loading, error, jobsByID }: NeedsAttentionProps) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const { data: resolved } = useResolved();
  const [additionalOpen, setAdditionalOpen] = useState(false);
  const [resolvedOpen, setResolvedOpen] = useState(false);
  const [expandedGroups, setExpandedGroups] = useState<Record<string, boolean>>({});

  const recurring = useMemo<PatternAnalysis[]>(
    () =>
      (report?.recurring_patterns ?? [])
        .filter((pattern) => pattern.job_id && !(pattern.id && resolved.resolved[pattern.id]))
        .slice(0, MAX_OVERVIEW_PATTERNS),
    [report, resolved],
  );

  const resolvedPatterns = useMemo<PatternAnalysis[]>(
    () =>
      (report?.recurring_patterns ?? []).filter(
        (pattern) => pattern.job_id && pattern.id && resolved.resolved[pattern.id],
      ),
    [report, resolved],
  );

  const groups = useMemo<ItemGroup[]>(() => {
    if (!report) return [];
    const broken = report.recently_broken ?? [];
    const persistent = report.persistent_failures ?? [];
    const flaky = report.most_flaky ?? [];
    const hasPrimary = broken.length > 0 || persistent.length > 0;

    if (hasPrimary) {
      let remaining = MAX_ITEMS;
      const result: ItemGroup[] = [];
      if (broken.length > 0) {
        const items = broken.slice(0, remaining);
        result.push({ label: "New regressions", items });
        remaining -= items.length;
      }
      if (persistent.length > 0 && remaining > 0) {
        result.push({ label: "Persistent failures", items: persistent.slice(0, remaining) });
      }
      return result;
    }
    return flaky.length > 0 ? [{ label: "Flaky tests", items: flaky.slice(0, MAX_ITEMS) }] : [];
  }, [report]);

  if (loading || error || !report) return null;

  if (recurring.length === 0 && groups.length === 0 && resolvedPatterns.length === 0) {
    return (
      <Box sx={{ borderBlock: "1px solid", borderColor: "divider", py: 5, textAlign: "center" }}>
        <CheckCircleOutlined sx={{ fontSize: 28, color: "success.main" }} />
        <Typography variant="headline" component="h2" sx={{ mt: 1, ...overviewTypography.majorHeading }}>All clear</Typography>
        <Typography variant="body2" color="text.secondary" sx={overviewTypography.primaryBody}>No tests currently need attention.</Typography>
      </Box>
    );
  }

  const totalItems = recurring.length + groups.reduce((sum, group) => sum + group.items.length, 0);
  const featured = recurring.slice(0, FEATURED_PATTERNS);
  const additional = recurring.slice(FEATURED_PATTERNS);

  return (
    <Box component="section" aria-labelledby="needs-attention-heading" sx={{ borderBlock: "1px solid", borderColor: "divider" }}>
      <Box
        sx={{
          minHeight: overviewLayout.majorBandMinHeight,
          display: "flex",
          alignItems: "center",
          gap: 1,
          px: 1.5,
          py: 1,
          bgcolor: "surface.containerHigh",
          boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
        }}
      >
        <ReportProblem color="warning" sx={{ fontSize: 20 }} />
        <Typography
          id="needs-attention-heading"
          variant="headline"
          component="h2"
          sx={overviewTypography.majorHeading}
        >
          Needs attention
        </Typography>
        <Typography variant="data" color="text.secondary" sx={{ ml: "auto", ...overviewTypography.data }}>
          {totalItems} active items
        </Typography>
      </Box>

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
            label="Additional recurring patterns"
            count={additional.length}
            open={additionalOpen}
            controls="additional-recurring-patterns"
            onClick={() => setAdditionalOpen((open) => !open)}
          />
          <Collapse id="additional-recurring-patterns" in={additionalOpen} timeout="auto" unmountOnExit>
            {additional.map((pattern) => {
              const refreshStatus = report.pattern_refresh?.jobs?.[pattern.job_id ?? ""];
              const stale = Boolean(refreshStatus && refreshStatus.state !== "current");
              return (
                <AttentionRow
                  key={pattern.id ?? pattern.job_id ?? pattern.subject}
                  to={jobPath(pattern.job_id ?? "")}
                  subject={shortJobName(pattern.subject, filePrefix)}
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
        const progressive = group.label === "Persistent failures" || group.label === "Flaky tests";
        const initialCount = group.label === "Persistent failures" ? 1 : group.label === "Flaky tests" ? 3 : group.items.length;
        const open = expandedGroups[group.label] ?? false;
        const visibleItems = progressive && !open ? group.items.slice(0, initialCount) : group.items;
        const remaining = Math.max(0, group.items.length - initialCount);
        const controls = `attention-group-${group.label.toLowerCase().replace(/\s+/g, "-")}`;
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
            <Box id={controls}>
              {visibleItems.map((item) => {
                const failing = item.classification !== "flaky";
                const consecutive = item.consecutive_failures > 0
                  ? `${item.consecutive_failures} consecutive ${item.consecutive_failures === 1 ? "failure" : "failures"}`
                  : undefined;
                return (
                  <AttentionRow
                    key={`${item.job_id}/${item.test_name}`}
                    to={item.last_failure?.build_id
                      ? testRunPath(item.job_id, item.test_name, item.last_failure.build_id)
                      : testPath(item.job_id, item.test_name)}
                    subject={shortJobName(item.job_name, filePrefix)}
                    summary={shortTestName(item.test_name)}
                    detail={item.last_failure?.failure_message}
                    count={consecutive}
                    signal={failing ? "Failing" : "Flaky"}
                    statusColor={failing ? "error" : "warning"}
                  />
                );
              })}
            </Box>
            {progressive && remaining > 0 && (
              <DisclosureButton
                label={open ? `Hide additional ${group.label.toLowerCase()}` : `Show additional ${group.label.toLowerCase()}`}
                count={remaining}
                open={open}
                controls={controls}
                onClick={() => setExpandedGroups((current) => ({ ...current, [group.label]: !open }))}
              />
            )}
          </Box>
        );
      })}

      {resolvedPatterns.length > 0 && (
        <Box component="section">
          <DisclosureButton
            label="Resolved patterns"
            count={resolvedPatterns.length}
            open={resolvedOpen}
            controls="resolved-patterns"
            onClick={() => setResolvedOpen((open) => !open)}
          />
          <Collapse id="resolved-patterns" in={resolvedOpen} timeout="auto" unmountOnExit>
            {resolvedPatterns.map((pattern) => {
              const entry = pattern.id ? resolved.resolved[pattern.id] : undefined;
              return (
                <AttentionRow
                  key={pattern.id ?? pattern.job_id ?? pattern.subject}
                  to={jobPath(pattern.job_id ?? "")}
                  subject={shortJobName(pattern.subject, filePrefix)}
                  summary={pattern.shared_root_cause || pattern.summary}
                  detail={entry?.note}
                  signal="Resolved"
                  statusColor="success"
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
