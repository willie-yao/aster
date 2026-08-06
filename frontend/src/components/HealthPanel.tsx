import Check from "@mui/icons-material/Check";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Typography from "@mui/material/Typography";
import { soft, type SoftColor } from "../theme";
import { countLabel } from "../lib/dashboardOverview";
import type { JobSummary } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";

interface HealthPanelProps {
  jobs: JobSummary[];
  onFilterClick?: (status: string) => void;
  activeFilter?: string;
}

export function HealthPanel({ jobs, onFilterClick, activeFilter }: HealthPanelProps) {
  const total = jobs.length;
  const rows: {
    label: string;
    status: "PASSING" | "FLAKY" | "FAILING";
    count: number;
    color: Extract<SoftColor, "success" | "warning" | "error">;
  }[] = [
    {
      label: "Passing",
      status: "PASSING",
      count: jobs.filter((job) => job.overall_status === "PASSING").length,
      color: "success",
    },
    {
      label: "Flaky",
      status: "FLAKY",
      count: jobs.filter((job) => job.overall_status === "FLAKY").length,
      color: "warning",
    },
    {
      label: "Failing",
      status: "FAILING",
      count: jobs.filter((job) => job.overall_status === "FAILING").length,
      color: "error",
    },
  ];

  return (
    <Box
      component="section"
      aria-labelledby="job-health-heading"
      sx={{
        display: "grid",
        gridTemplateColumns: { xs: "repeat(3, minmax(0, 1fr))", md: "minmax(180px, 1fr) repeat(3, minmax(120px, 170px))" },
        borderBlock: "1px solid",
        borderColor: "divider",
      }}
    >
      <Box
        sx={{
          gridColumn: { xs: "1 / -1", md: "auto" },
          minHeight: 70,
          display: "flex",
          flexDirection: "column",
          justifyContent: "center",
          px: { xs: 1.5, md: 2 },
        }}
      >
        <Typography
          id="job-health-heading"
          variant="label"
          component="h2"
          color="text.secondary"
          sx={overviewTypography.subsectionHeading}
        >
          Job health
        </Typography>
        <Typography variant="stat" component="span" sx={{ mt: 0.25, fontSize: "21px", lineHeight: "28px" }}>
          {countLabel(total, "job")}
        </Typography>
      </Box>

      {rows.map((row) => {
        const active = activeFilter === row.status;
        const percentage = total === 0 ? 0 : Math.round((row.count / total) * 100);
        return (
          <ButtonBase
            key={row.status}
            onClick={() => onFilterClick?.(active ? "ALL" : row.status)}
            disabled={!onFilterClick}
            aria-pressed={active}
            aria-label={`${row.label}: ${countLabel(row.count, "job")}, ${percentage}%`}
            sx={{
              position: "relative",
              minWidth: 0,
              minHeight: 70,
              display: "grid",
              gridTemplateColumns: "auto minmax(0, 1fr) auto",
              alignItems: "center",
              gap: 1,
              px: { xs: 1, sm: 1.5 },
              py: 1,
              border: 0,
              borderTop: { xs: "1px solid", md: 0 },
              borderLeft: "1px solid",
              borderColor: "divider",
              textAlign: "left",
              bgcolor: (theme) => (active ? soft(theme, "primary", 0.12) : "transparent"),
              boxShadow: active ? "inset 0 -3px 0 var(--mui-palette-primary-main)" : "none",
              "&:hover": { bgcolor: active ? (theme) => soft(theme, "primary", 0.16) : "surface.containerHigh" },
              "&.Mui-focusVisible": {
                borderLeftColor: "divider",
                outline: "2px solid",
                outlineColor: "primary.main",
                outlineOffset: -2,
              },
            }}
          >
            <Box aria-hidden="true" sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: `${row.color}.main` }} />
            <Box sx={{ minWidth: 0 }}>
              <Box sx={{ display: "flex", alignItems: "center", gap: 0.5 }}>
                <Typography variant="body2" component="span" sx={{ fontWeight: 600, ...overviewTypography.primaryBody }}>
                  {row.label}
                </Typography>
                {active && <Check aria-hidden="true" sx={{ fontSize: 15, color: "primary.main" }} />}
              </Box>
              <Typography variant="data" component="span" color="text.secondary" sx={overviewTypography.data}>
                {percentage}%
              </Typography>
            </Box>
            <Typography variant="stat" component="span" sx={{ fontSize: "19px", color: `${row.color}.main` }}>
              {row.count}
            </Typography>
          </ButtonBase>
        );
      })}
    </Box>
  );
}
