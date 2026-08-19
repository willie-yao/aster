import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { JobSummary } from "../types/dashboard";
import { formatDuration, formatPercent, timeAgo } from "../lib/utils";
import { jobPath } from "../lib/routes";
import { Sparkline } from "./Sparkline";
import { StatusChip } from "./StatusChip";
import { overviewLayout, overviewTypography } from "../theme/overview";

export interface JobHealthSection {
  id: string;
  label?: string;
  jobs: JobSummary[];
}

interface JobHealthTableProps {
  sections: JobHealthSection[];
}

const desktopBreakpoint = "@media (min-width: 1024px)";
const wideBreakpoint = "@media (min-width: 1200px)";
const compactColumns = "minmax(210px, 2fr) 76px 288px 76px 78px 58px 82px";
const wideColumns = "minmax(280px, 2.4fr) 104px 288px 88px 96px 64px 88px";
const headers = ["Job", "Branch", "Recent runs", "Last 10 pass", "Last run", "Duration", "Current"];

function jobValues(job: JobSummary) {
  return {
    displayName: job.tab_name || job.name,
    lastRun: job.last_run ? timeAgo(job.last_run.timestamp) : "Not available",
    duration: job.last_run?.duration_seconds != null
      ? formatDuration(job.last_run.duration_seconds)
      : "Not available",
  };
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={{ display: "inline-flex", alignItems: "baseline", gap: 0.5 }}>
      <Typography variant="caption" component="span" color="textSecondary" sx={overviewTypography.description}>
        {label}
      </Typography>
      <Typography variant="data" component="span" color="textPrimary" sx={overviewTypography.data}>
        {value}
      </Typography>
    </Box>
  );
}

function JobLink({ job, compact }: { job: JobSummary; compact: boolean }) {
  const { displayName } = jobValues(job);
  return (
    <Box sx={{ minWidth: 0 }}>
      <Link
        component={RouterLink}
        to={jobPath(job.job_id)}
        underline="none"
        title={displayName}
        sx={{
          minWidth: 0,
          minHeight: compact ? 44 : 0,
          display: compact ? "flex" : "block",
          alignItems: compact ? "center" : undefined,
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          color: "text.primary",
          ...overviewTypography.jobIdentifier,
          "&:hover": { color: "primary.main", textDecoration: "underline" },
          "&:focus-visible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: 1,
          },
        }}
      >
        {displayName}
      </Link>
      {!compact && job.description && (
        <Typography
          variant="caption"
          color="textSecondary"
          title={job.description}
          sx={{
            display: "none",
            mt: 0.25,
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            ...overviewTypography.description,
            [wideBreakpoint]: { display: "block" },
          }}
        >
          {job.description}
        </Typography>
      )}
    </Box>
  );
}

function DesktopJobRow({ job }: { job: JobSummary }) {
  const { lastRun, duration } = jobValues(job);
  return (
    <Box
      role="row"
      sx={{
        minHeight: overviewLayout.ledgerRowMinHeight,
        display: "grid",
        gridTemplateColumns: compactColumns,
        gridTemplateAreas: '"job branch runs pass last duration status"',
        alignItems: "center",
        columnGap: 1,
        px: 1.5,
        py: 1,
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-within": { boxShadow: "inset 2px 0 0 var(--mui-palette-primary-main)" },
        [wideBreakpoint]: {
          gridTemplateColumns: wideColumns,
          columnGap: 1.5,
          px: 2,
        },
      }}
    >
      <Box role="cell" sx={{ gridArea: "job", minWidth: 0 }}><JobLink job={job} compact={false} /></Box>
      <Typography role="cell" variant="data" color="textSecondary" title={job.branch} sx={{ gridArea: "branch", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", ...overviewTypography.data }}>
        {job.branch || "Not set"}
      </Typography>
      <Box role="cell" sx={{ gridArea: "runs", minWidth: 0 }}><Sparkline runs={job.recent_runs} jobID={job.job_id} /></Box>
      <Typography role="cell" variant="data" sx={{ gridArea: "pass", ...overviewTypography.data }}>{formatPercent(job.pass_rate_recent)}</Typography>
      <Typography role="cell" variant="data" color="textSecondary" sx={{ gridArea: "last", ...overviewTypography.data }}>{lastRun}</Typography>
      <Typography role="cell" variant="data" color="textSecondary" sx={{ gridArea: "duration", ...overviewTypography.data }}>{duration}</Typography>
      <Box role="cell" sx={{ gridArea: "status", justifySelf: "end" }}>
        <StatusChip status={job.current_status} sx={{ height: 26, fontSize: "13px" }} />
      </Box>
    </Box>
  );
}

function MobileJobRow({ job }: { job: JobSummary }) {
  const { lastRun, duration } = jobValues(job);
  return (
    <Box
      role="listitem"
      sx={{
        display: "grid",
        gridTemplateColumns: "minmax(0, 1fr) auto",
        gridTemplateAreas: '"job status" "meta meta" "runs runs"',
        alignItems: "center",
        columnGap: 1,
        rowGap: 0.75,
        px: 1.5,
        py: 1,
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-within": { boxShadow: "inset 2px 0 0 var(--mui-palette-primary-main)" },
      }}
    >
      <Box sx={{ gridArea: "job", minWidth: 0 }}><JobLink job={job} compact /></Box>
      <Box sx={{ gridArea: "status", justifySelf: "end" }}>
        <StatusChip status={job.current_status} sx={{ height: 26, fontSize: "13px" }} />
      </Box>
      <Box sx={{ gridArea: "meta", display: "flex", minWidth: 0, alignItems: "center", gap: 1.5, flexWrap: "wrap" }}>
        <Metric label="Branch" value={job.branch || "Not set"} />
        <Metric label="Last 10 pass" value={formatPercent(job.pass_rate_recent)} />
        <Metric label="Last" value={lastRun} />
        <Metric label="Duration" value={duration} />
      </Box>
      <Box sx={{ gridArea: "runs", minWidth: 0 }}><Sparkline runs={job.recent_runs} jobID={job.job_id} /></Box>
    </Box>
  );
}

function CategoryBand({ section, headingID }: { section: JobHealthSection; headingID: string }) {
  return (
    <Box
      sx={{
        minHeight: overviewLayout.categoryBandMinHeight,
        display: "flex",
        alignItems: "baseline",
        gap: 1,
        px: 1.5,
        py: 1,
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.containerHigh",
        boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
      }}
    >
      <Typography id={headingID} variant="headline" component="h3" sx={overviewTypography.categoryHeading}>
        {section.label}
      </Typography>
      <Typography variant="data" color="textSecondary" sx={overviewTypography.data}>
        {section.jobs.length} {section.jobs.length === 1 ? "job" : "jobs"}
      </Typography>
    </Box>
  );
}

export function JobHealthTable({ sections }: JobHealthTableProps) {
  return (
    <>
      <Box aria-label="Job health" sx={{ borderBlock: "1px solid", borderColor: "divider", bgcolor: "surface.container", [desktopBreakpoint]: { display: "none" } }}>
        {sections.map((section) => {
          const headingID = `job-category-mobile-${section.id}`;
          return (
            <Box key={section.id} component="section" aria-labelledby={section.label ? headingID : undefined}>
              {section.label && <CategoryBand section={section} headingID={headingID} />}
              <Box role="list" aria-label={section.label ? `${section.label} jobs` : "Jobs"}>
                {section.jobs.map((job) => <MobileJobRow key={job.job_id} job={job} />)}
              </Box>
            </Box>
          );
        })}
      </Box>

      <Box role="table" aria-label="Job health" sx={{ display: "none", borderBlock: "1px solid", borderColor: "divider", bgcolor: "surface.container", [desktopBreakpoint]: { display: "block" } }}>
        <Box role="row" sx={{ display: "grid", gridTemplateColumns: compactColumns, alignItems: "center", columnGap: 1, px: 1.5, py: 1, minHeight: 42, borderBottom: "1px solid", borderColor: "divider", bgcolor: "surface.containerHigh", [wideBreakpoint]: { gridTemplateColumns: wideColumns, columnGap: 1.5, px: 2 } }}>
          {headers.map((header) => (
            <Typography key={header} role="columnheader" variant="label" color="textSecondary" sx={overviewTypography.tableHeading}>
              {header}
            </Typography>
          ))}
        </Box>
        {sections.map((section) => {
          const headingID = `job-category-desktop-${section.id}`;
          return (
            <Box key={section.id} role="rowgroup">
              {section.label && (
                <Box role="row">
                  <Box role="cell" aria-colspan={7}>
                    <CategoryBand section={section} headingID={headingID} />
                  </Box>
                </Box>
              )}
              {section.jobs.map((job) => <DesktopJobRow key={job.job_id} job={job} />)}
            </Box>
          );
        })}
      </Box>
    </>
  );
}
