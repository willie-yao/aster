import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Typography from "@mui/material/Typography";
import { Panel } from "./Panel";
import { soft, type SoftColor } from "../theme";
import type { JobSummary } from "../types/dashboard";

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
    <Panel sx={{ borderRadius: "6px", p: 2 }}>
      <Box sx={{ display: "flex", alignItems: "baseline", justifyContent: "space-between", gap: 2, mb: 1.5 }}>
        <Typography variant="headline" component="h2">
          Health
        </Typography>
        <Typography variant="data" color="text.secondary">
          {total} {total === 1 ? "job" : "jobs"}
        </Typography>
      </Box>

      <Box sx={{ display: "grid", gridTemplateColumns: { xs: "repeat(3, minmax(0, 1fr))", lg: "1fr" }, gap: 1 }}>
        {rows.map((row) => {
          const active = activeFilter === row.status;
          const percentage = total === 0 ? 0 : Math.round((row.count / total) * 100);
          return (
            <ButtonBase
              key={row.status}
              onClick={() => onFilterClick?.(active ? "ALL" : row.status)}
              disabled={!onFilterClick}
              aria-pressed={active}
              aria-label={`${row.label}: ${row.count} jobs, ${percentage}%`}
              sx={{
                minWidth: 0,
                minHeight: 64,
                display: "grid",
                gridTemplateColumns: { xs: "1fr", lg: "auto minmax(0, 1fr) auto" },
                alignItems: "center",
                gap: { xs: 0.5, lg: 1 },
                px: 1.25,
                py: 1,
                border: "1px solid",
                borderColor: active ? "primary.main" : "divider",
                borderRadius: "4px",
                textAlign: { xs: "center", lg: "left" },
                bgcolor: (theme) => (active ? soft(theme, "primary", 0.08) : "transparent"),
                "&:hover": { bgcolor: "surface.containerHigh" },
                "&.Mui-focusVisible": {
                  outline: "2px solid",
                  outlineColor: "primary.main",
                  outlineOffset: 1,
                },
              }}
            >
              <Box
                aria-hidden="true"
                sx={{
                  width: 8,
                  height: 8,
                  borderRadius: "2px",
                  bgcolor: `${row.color}.main`,
                  justifySelf: { xs: "center", lg: "start" },
                }}
              />
              <Box sx={{ minWidth: 0 }}>
                <Typography variant="body2" component="span" sx={{ display: "block", fontWeight: 600 }}>
                  {row.label}
                </Typography>
                <Typography variant="data" component="span" color="text.secondary">
                  {percentage}%
                </Typography>
              </Box>
              <Typography variant="stat" component="span" sx={{ fontSize: "1.125rem", color: `${row.color}.main` }}>
                {row.count}
              </Typography>
            </ButtonBase>
          );
        })}
      </Box>
    </Panel>
  );
}
