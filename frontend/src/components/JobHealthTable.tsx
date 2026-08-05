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
const compactColumns = "minmax(210px, 2fr) 76px 174px 56px 78px 58px 82px";
const wideColumns = "minmax(280px, 2.4fr) 104px 192px 64px 96px 64px 88px";
const headers = ["Job", "Branch", "Recent runs", "Pass", "Last run", "Duration", "Status"];

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={{ display: "inline-flex", alignItems: "baseline", gap: 0.5 }}>
      <Typography variant="caption" component="span" color="text.secondary" sx={overviewTypography.description}>
        {label}
      </Typography>
      <Typography variant="data" component="span" color="text.primary" sx={overviewTypography.data}>
        {value}
      </Typography>
    </Box>
  );
}

function JobHealthRow({ job }: { job: JobSummary }) {
  const displayName = job.tab_name || job.name;
  const lastRun = job.last_run ? timeAgo(job.last_run.timestamp) : "Not available";
  const duration = job.last_run?.duration_seconds != null
    ? formatDuration(job.last_run.duration_seconds)
    : "Not available";

  return (
    <Box
      role="row"
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
        [desktopBreakpoint]: {
          minHeight: overviewLayout.ledgerRowMinHeight,
          gridTemplateColumns: compactColumns,
          gridTemplateAreas: '"job branch runs pass last duration status"',
          columnGap: 1,
          rowGap: 0,
          px: 1.5,
          py: 1,
        },
        [wideBreakpoint]: {
          gridTemplateColumns: wideColumns,
          columnGap: 1.5,
          px: 2,
        },
      }}
    >
      <Box role="cell" sx={{ gridArea: "job", minWidth: 0 }}>
        <Link
          component={RouterLink}
          to={jobPath(job.job_id)}
          underline="none"
          title={displayName}
          sx={{
            minWidth: 0,
            minHeight: 44,
            display: "flex",
            alignItems: "center",
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
            [desktopBreakpoint]: { minHeight: 0, display: "block" },
          }}
        >
          {displayName}
        </Link>
        {job.description && (
          <Typography
            variant="caption"
            color="text.secondary"
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

      <Typography
        role="cell"
        variant="data"
        color="text.secondary"
        title={job.branch}
        sx={{
          gridArea: "branch",
          display: "none",
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          ...overviewTypography.data,
          [desktopBreakpoint]: { display: "block" },
        }}
      >
        {job.branch || "Not set"}
      </Typography>

      <Box role="cell" sx={{ gridArea: "runs", minWidth: 0 }}>
        <Sparkline runs={job.recent_runs} jobID={job.job_id} />
      </Box>

      <Typography role="cell" variant="data" sx={{ gridArea: "pass", display: "none", ...overviewTypography.data, [desktopBreakpoint]: { display: "block" } }}>
        {formatPercent(job.pass_rate_recent)}
      </Typography>
      <Typography role="cell" variant="data" color="text.secondary" sx={{ gridArea: "last", display: "none", ...overviewTypography.data, [desktopBreakpoint]: { display: "block" } }}>
        {lastRun}
      </Typography>
      <Typography role="cell" variant="data" color="text.secondary" sx={{ gridArea: "duration", display: "none", ...overviewTypography.data, [desktopBreakpoint]: { display: "block" } }}>
        {duration}
      </Typography>
      <Box role="cell" sx={{ gridArea: "status", justifySelf: "end" }}>
        <StatusChip status={job.overall_status} sx={{ height: 26, fontSize: "0.8125rem" }} />
      </Box>

      <Box
        role="cell"
        sx={{
          gridArea: "meta",
          display: "flex",
          minWidth: 0,
          alignItems: "center",
          gap: 1.5,
          flexWrap: "wrap",
          [desktopBreakpoint]: { display: "none" },
        }}
      >
        <Metric label="Branch" value={job.branch || "Not set"} />
        <Metric label="Pass" value={formatPercent(job.pass_rate_recent)} />
        <Metric label="Last" value={lastRun} />
        <Metric label="Duration" value={duration} />
      </Box>
    </Box>
  );
}

export function JobHealthTable({ sections }: JobHealthTableProps) {
  return (
    <Box
      role="table"
      aria-label="Job health"
      sx={{
        borderBlock: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
      }}
    >
      <Box
        role="row"
        sx={{
          display: "none",
          alignItems: "center",
          columnGap: 1,
          px: 1.5,
          py: 1,
          minHeight: 42,
          borderBottom: "1px solid",
          borderColor: "divider",
          bgcolor: "surface.containerHigh",
          [desktopBreakpoint]: { display: "grid", gridTemplateColumns: compactColumns },
          [wideBreakpoint]: { gridTemplateColumns: wideColumns, columnGap: 1.5, px: 2 },
        }}
      >
        {headers.map((header) => (
          <Typography
            key={header}
            role="columnheader"
            variant="label"
            color="text.secondary"
            sx={overviewTypography.tableHeading}
          >
            {header}
          </Typography>
        ))}
      </Box>

      {sections.map((section) => (
        <Box key={section.id} role="rowgroup">
          {section.label && (
            <Box
              role="row"
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
              <Typography
                role="cell"
                aria-colspan={7}
                variant="headline"
                component="h3"
                sx={overviewTypography.categoryHeading}
              >
                {section.label}
              </Typography>
              <Typography variant="data" color="text.secondary" sx={overviewTypography.data}>
                {section.jobs.length} {section.jobs.length === 1 ? "job" : "jobs"}
              </Typography>
            </Box>
          )}
          {section.jobs.map((job) => (
            <JobHealthRow key={job.job_id} job={job} />
          ))}
        </Box>
      ))}
    </Box>
  );
}
