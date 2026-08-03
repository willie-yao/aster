import Box from "@mui/material/Box";
import Card from "@mui/material/Card";
import CardActionArea from "@mui/material/CardActionArea";
import CardContent from "@mui/material/CardContent";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { JobSummary } from "../types/dashboard";
import { jobPath } from "../lib/routes";
import { formatPercent, timeAgo, formatDuration } from "../lib/utils";
import { statusAccent } from "../theme";
import { StatusChip } from "./StatusChip";
import { Sparkline } from "./Sparkline";

interface JobCardProps {
  job: JobSummary;
}

export function JobCard({ job }: JobCardProps) {
  const lastRunTime = job.last_run ? timeAgo(job.last_run.timestamp) : "—";
  const lastDuration =
    job.last_run?.duration_seconds != null
      ? formatDuration(job.last_run.duration_seconds)
      : "—";

  const footerItems = [
    { label: "Pass", value: formatPercent(job.pass_rate_recent), tooltip: "Pass rate over the last 10 runs" },
    { label: "Last", value: lastRunTime },
    { label: "Dur", value: lastDuration },
  ];

  return (
    <Card
      elevation={0}
      sx={(theme) => {
        const accent = statusAccent(theme, job.overall_status);
        return {
          position: "relative",
          height: "100%",
          borderRadius: "16px",
          bgcolor: (theme.vars ?? theme).palette.surface.glass,
          backdropFilter: "blur(14px)",
          WebkitBackdropFilter: "blur(14px)",
          border: "1px solid",
          borderColor: accent.border,
          boxShadow: accent.glow,
          backgroundImage: "none",
          overflow: "hidden",
          transition: "transform 180ms ease, border-color 180ms ease, box-shadow 180ms ease",
          // Status accent bar down the leading edge.
          "&::before": {
            content: '""',
            position: "absolute",
            insetBlock: 0,
            insetInlineStart: 0,
            width: 3,
            bgcolor: accent.bar,
          },
          "@media (hover: hover)": {
            "&:hover": {
              transform: "translateY(-3px)",
              borderColor: accent.hoverBorder,
              boxShadow: accent.hoverGlow,
            },
          },
        };
      }}
    >
      <CardActionArea
        component={RouterLink}
        to={jobPath(job.job_id)}
        sx={{ height: "100%", display: "flex", alignItems: "stretch" }}
      >
        <CardContent
          sx={{
            width: "100%",
            display: "flex",
            flexDirection: "column",
            gap: 1.5,
            p: 2,
            pl: 2.25,
            "&:last-child": { pb: 2 },
          }}
        >
          <Box sx={{ display: "flex", alignItems: "flex-start", gap: 1.5 }}>
            <Typography
              variant="headline"
              component="h3"
              sx={{
                flex: 1,
                minWidth: 0,
                fontSize: "0.875rem",
                lineHeight: 1.35,
                color: "text.primary",
                transition: "color 160ms ease",
                ".MuiCardActionArea-root:hover &": { color: "primary.main" },
              }}
            >
              {job.tab_name || job.name}
            </Typography>
            <StatusChip status={job.overall_status} />
          </Box>

          {job.description && (
            <Typography
              variant="caption"
              color="text.secondary"
              sx={{
                lineHeight: 1.6,
                display: "-webkit-box",
                WebkitLineClamp: 2,
                WebkitBoxOrient: "vertical",
                overflow: "hidden",
              }}
            >
              {job.description}
            </Typography>
          )}

          <Sparkline runs={job.recent_runs} jobID={job.job_id} />

          <Box
            sx={{
              mt: "auto",
              pt: 1.5,
              borderTop: "1px solid",
              borderColor: "divider",
              display: "flex",
              alignItems: "center",
              gap: 2.5,
              flexWrap: "wrap",
            }}
          >
            {footerItems.map((item) => {
              const content = (
                <Box
                  key={item.label}
                  sx={{ display: "flex", alignItems: "baseline", gap: 0.75 }}
                >
                  <Typography
                    variant="label"
                    component="span"
                    color="text.secondary"
                    sx={{ textTransform: "uppercase", fontSize: "0.625rem" }}
                  >
                    {item.label}
                  </Typography>
                  <Typography
                    variant="data"
                    component="span"
                    sx={{ color: "text.primary", fontSize: "0.75rem" }}
                  >
                    {item.value}
                  </Typography>
                </Box>
              );
              return item.tooltip ? (
                <Tooltip key={item.label} title={item.tooltip}>
                  {content}
                </Tooltip>
              ) : (
                content
              );
            })}
          </Box>
        </CardContent>
      </CardActionArea>
    </Card>
  );
}
