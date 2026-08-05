import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import { Link as RouterLink } from "react-router-dom";
import type { RunSummary } from "../types/dashboard";
import { jobRunPath } from "../lib/routes";
import { dotColorFor } from "../theme";

interface SparklineProps {
  runs: RunSummary[];
  jobID: string;
}

export function Sparkline({ runs, jobID }: SparklineProps) {
  const recent = runs.slice(0, 8).reverse();

  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 0.25 }}>
      {recent.map((run) => {
        const label =
          run.result === "PENDING" ? "Running" : run.passed ? "Passed" : "Failed";
        return (
          <Tooltip key={run.build_id} title={`#${run.build_id} - ${label}`}>
            <Box
              component={RouterLink}
              to={jobRunPath(jobID, run.build_id)}
              aria-label={`Run ${run.build_id} ${label.toLowerCase()}`}
              onClick={(event) => event.stopPropagation()}
              sx={{
                width: { xs: 32, sm: 24 },
                height: { xs: 32, sm: 24 },
                display: "inline-flex",
                alignItems: "center",
                justifyContent: "center",
                borderRadius: "4px",
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
