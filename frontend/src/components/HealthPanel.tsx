import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Typography from "@mui/material/Typography";
import { Panel } from "./Panel";
import { HealthDonut } from "./HealthDonut";
import { soft, type SoftColor } from "../theme";
import type { JobSummary } from "../types/dashboard";

interface HealthPanelProps {
  jobs: JobSummary[];
  onFilterClick?: (status: string) => void;
  activeFilter?: string;
}

// At-a-glance health widget: a proportional donut beside clickable status rows.
// Rows toggle the dashboard status filter, mirroring the filter chips.
export function HealthPanel({ jobs, onFilterClick, activeFilter }: HealthPanelProps) {
  const total = jobs.length || 1;
  const passing = jobs.filter((j) => j.overall_status === "PASSING").length;
  const flaky = jobs.filter((j) => j.overall_status === "FLAKY").length;
  const failing = jobs.filter((j) => j.overall_status === "FAILING").length;

  const rows: {
    label: string;
    status: "PASSING" | "FLAKY" | "FAILING";
    count: number;
    color: Extract<SoftColor, "success" | "warning" | "error">;
  }[] = [
    { label: "Passing", status: "PASSING", count: passing, color: "success" },
    { label: "Flaky", status: "FLAKY", count: flaky, color: "warning" },
    { label: "Failing", status: "FAILING", count: failing, color: "error" },
  ];

  return (
    <Panel sx={{ borderRadius: "16px", p: { xs: 2, sm: 2.5 }, height: "100%", display: "flex", flexDirection: "column" }}>
      <Typography
        variant="label"
        component="h2"
        color="text.secondary"
        sx={{ textTransform: "uppercase", display: "block", mb: 2, flexShrink: 0 }}
      >
        Health
      </Typography>

      <Box
        sx={{
          flex: 1,
          minHeight: 0,
          display: "flex",
          alignItems: "center",
          gap: { xs: 2, sm: 3 },
          flexWrap: { xs: "wrap", sm: "nowrap" },
        }}
      >
        <HealthDonut passing={passing} flaky={flaky} failing={failing} />

        <Box sx={{ flex: 1, minWidth: 160, display: "flex", flexDirection: "column", gap: 0.5 }}>
          {rows.map((row) => {
            const isActive = activeFilter === row.status;
            const pct = Math.round((row.count / total) * 100);
            return (
              <ButtonBase
                key={row.status}
                onClick={() => onFilterClick?.(isActive ? "ALL" : row.status)}
                disabled={!onFilterClick}
                aria-pressed={isActive}
                sx={{
                  display: "flex",
                  alignItems: "center",
                  gap: 1.25,
                  width: "100%",
                  px: 1,
                  py: 0.75,
                  borderRadius: "10px",
                  textAlign: "left",
                  cursor: onFilterClick ? "pointer" : "default",
                  border: "1px solid",
                  borderColor: isActive ? (theme) => soft(theme, row.color, 0.5) : "transparent",
                  bgcolor: (theme) => (isActive ? soft(theme, row.color, 0.1) : "transparent"),
                  transition: "background-color 150ms ease, border-color 150ms ease",
                  "@media (hover: hover)": {
                    "&:hover": {
                      bgcolor: (theme) => (onFilterClick ? soft(theme, row.color, 0.08) : "transparent"),
                    },
                  },
                }}
              >
                <Box
                  sx={{
                    width: 9,
                    height: 9,
                    borderRadius: "50%",
                    flexShrink: 0,
                    bgcolor: `${row.color}.main`,
                    boxShadow: (theme) => `0 0 8px ${soft(theme, row.color, 0.7)}`,
                  }}
                />
                <Typography variant="body2" sx={{ flex: 1, fontWeight: 600, color: "text.primary" }}>
                  {row.label}
                </Typography>
                <Typography
                  variant="data"
                  component="span"
                  sx={{ fontSize: "1rem", fontWeight: 700, color: `${row.color}.main` }}
                >
                  {row.count}
                </Typography>
                <Typography
                  variant="data"
                  component="span"
                  color="text.secondary"
                  sx={{ width: 40, textAlign: "right", fontSize: "0.75rem" }}
                >
                  {pct}%
                </Typography>
              </ButtonBase>
            );
          })}
        </Box>
      </Box>
    </Panel>
  );
}
