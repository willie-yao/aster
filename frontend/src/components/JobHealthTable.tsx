import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { JobSummary } from "../types/dashboard";
import { formatDuration, formatPercent, timeAgo } from "../lib/utils";
import { jobPath } from "../lib/routes";
import { Sparkline } from "./Sparkline";
import { StatusChip } from "./StatusChip";

interface JobHealthTableProps {
  jobs: JobSummary[];
}

const desktopColumns = {
  md: "minmax(240px, 2fr) minmax(96px, 0.7fr) 200px 72px 104px 88px",
  lg: "minmax(300px, 2.4fr) minmax(110px, 0.8fr) 200px 72px 112px 72px 88px",
};

const headers = ["Job", "Branch", "Recent runs", "Pass rate", "Last run", "Duration", "Status"];

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={{ display: "inline-flex", alignItems: "baseline", gap: 0.5 }}>
      <Typography variant="caption" component="span" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="data" component="span" color="text.primary">
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
        display: { xs: "grid", md: "grid" },
        gridTemplateColumns: { xs: "minmax(0, 1fr) auto", ...desktopColumns },
        gridTemplateAreas: {
          xs: '"job status" "meta meta" "runs runs"',
          md: '"job branch runs pass last status"',
          lg: '"job branch runs pass last duration status"',
        },
        alignItems: "center",
        columnGap: { md: 1.5 },
        rowGap: { xs: 1, md: 0 },
        px: { xs: 1.5, md: 2 },
        py: { xs: 1.5, md: 1.25 },
        borderBottom: "1px solid",
        borderColor: "divider",
        bgcolor: "surface.container",
        transition: "background-color 140ms ease",
        "&:last-child": { borderBottom: 0 },
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-within": { boxShadow: "inset 2px 0 0 var(--mui-palette-primary-main)" },
      }}
    >
      <Box role="cell" sx={{ gridArea: "job", minWidth: 0 }}>
        <Link
          component={RouterLink}
          to={jobPath(job.job_id)}
          underline="none"
          title={displayName}
          sx={{
            display: "block",
            minWidth: 0,
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            color: "text.primary",
            fontFamily: "monospace",
            fontSize: "0.8125rem",
            fontWeight: 600,
            "&:hover": { color: "primary.main", textDecoration: "underline" },
            "&:focus-visible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: 2,
            },
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
              display: { xs: "none", lg: "block" },
              mt: 0.25,
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
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
          display: { xs: "none", md: "block" },
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
        }}
      >
        {job.branch || "Not set"}
      </Typography>

      <Box role="cell" sx={{ gridArea: "runs", minWidth: 0 }}>
        <Sparkline runs={job.recent_runs} jobID={job.job_id} />
      </Box>

      <Typography role="cell" variant="data" sx={{ gridArea: "pass", display: { xs: "none", md: "block" } }}>
        {formatPercent(job.pass_rate_recent)}
      </Typography>
      <Typography role="cell" variant="data" color="text.secondary" sx={{ gridArea: "last", display: { xs: "none", md: "block" } }}>
        {lastRun}
      </Typography>
      <Typography role="cell" variant="data" color="text.secondary" sx={{ gridArea: "duration", display: { xs: "none", lg: "block" } }}>
        {duration}
      </Typography>
      <Box role="cell" sx={{ gridArea: "status", justifySelf: "end" }}>
        <StatusChip status={job.overall_status} />
      </Box>

      <Box
        role="cell"
        sx={{
          gridArea: "meta",
          display: { xs: "flex", md: "none" },
          minWidth: 0,
          alignItems: "center",
          gap: 1.5,
          flexWrap: "wrap",
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

export function JobHealthTable({ jobs }: JobHealthTableProps) {
  return (
    <Box
      role="table"
      aria-label="Job health"
      sx={{
        overflow: "hidden",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "6px",
        bgcolor: "surface.container",
      }}
    >
      <Box
        role="row"
        sx={{
          display: { xs: "none", md: "grid" },
          gridTemplateColumns: desktopColumns,
          alignItems: "center",
          columnGap: 1.5,
          px: 2,
          py: 1,
          borderBottom: "1px solid",
          borderColor: "divider",
          bgcolor: "surface.containerHigh",
        }}
      >
        {headers.map((header) => (
          <Typography
            key={header}
            role="columnheader"
            variant="label"
            color="text.secondary"
            sx={{ display: header === "Duration" ? { md: "none", lg: "block" } : "block" }}
          >
            {header}
          </Typography>
        ))}
      </Box>
      <Box role="rowgroup">
        {jobs.map((job) => (
          <JobHealthRow key={job.job_id} job={job} />
        ))}
      </Box>
    </Box>
  );
}
