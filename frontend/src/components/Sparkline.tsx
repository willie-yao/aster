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
  // recent_runs is newest-first; reverse the displayed subset so newest is rightmost.
  const recent = runs.slice(0, 8).reverse();

  return (
    <Box sx={{ display: "flex", alignItems: "center", gap: 0.75 }}>
      {recent.map((run) => {
        const label =
          run.result === "PENDING" ? "Running" : run.passed ? "Passed" : "Failed";
        return (
          <Tooltip key={run.build_id} title={`#${run.build_id} — ${label}`}>
            <Box
              component={RouterLink}
              to={jobRunPath(jobID, run.build_id)}
              aria-label={`Run ${run.build_id} ${label.toLowerCase()}`}
              onClick={(event) => event.stopPropagation()}
              sx={{
                display: "inline-flex",
                alignItems: "center",
                justifyContent: "center",
                p: 0.5,
                m: -0.5,
                borderRadius: "50%",
                "&:hover > span": { transform: "scale(1.25)" },
              }}
            >
              <Box
                component="span"
                sx={{
                  display: "block",
                  width: 10,
                  height: 10,
                  borderRadius: "50%",
                  bgcolor: (theme) => dotColorFor(theme, run.passed, run.result),
                  transition: "transform 140ms ease",
                }}
              />
            </Box>
          </Tooltip>
        );
      })}
    </Box>
  );
}
