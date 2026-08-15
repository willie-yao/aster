import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import { Link as RouterLink } from "react-router-dom";
import type { RunSummary } from "../types/dashboard";
import { jobRunPath } from "../lib/routes";
import { formatAccessibleDate } from "../lib/utils";
import { dotColorFor } from "../theme";

interface SparklineProps {
  runs: RunSummary[];
  jobID: string;
}

export function Sparkline({ runs, jobID }: SparklineProps) {
  const recent = runs.slice(0, 8).reverse();

  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: { xs: "repeat(4, 44px)", sm: "repeat(8, 44px)" },
        alignItems: "center",
        gap: 0,
        "@media (min-width: 1024px)": {
          gridTemplateColumns: "repeat(8, 20px)",
          gap: "2px",
        },
        "@media (min-width: 1200px)": {
          gridTemplateColumns: "repeat(8, 22px)",
        },
      }}
    >
      {recent.map((run) => {
        const label = run.result === "PENDING" ? "Running" : run.passed ? "Passed" : "Failed";
        const date = formatAccessibleDate(run.timestamp);
        const context = `Run ${run.build_id}, ${label.toLowerCase()}, ${date}`;
        return (
          <Tooltip key={run.build_id} title={`#${run.build_id} - ${label} - ${date}`}>
            <Box
              component={RouterLink}
              to={jobRunPath(jobID, run.build_id)}
              aria-label={context}
              onClick={(event) => event.stopPropagation()}
              sx={{
                width: 44,
                height: 44,
                display: "inline-flex",
                alignItems: "center",
                justifyContent: "center",
                borderRadius: "4px",
                "@media (min-width: 1024px)": { width: 20, height: 28 },
                "@media (min-width: 1200px)": { width: 22 },
                "&:hover": { bgcolor: "surface.containerHigh" },
                "&:focus-visible": {
                  outline: "2px solid",
                  outlineColor: "primary.main",
                  outlineOffset: -2,
                },
              }}
            >
              <Box
                component="span"
                sx={{
                  display: "block",
                  width: 8,
                  height: 8,
                  borderRadius: "2px",
                  bgcolor: (theme) => dotColorFor(theme, run.passed, run.result),
                }}
              />
            </Box>
          </Tooltip>
        );
      })}
    </Box>
  );
}
