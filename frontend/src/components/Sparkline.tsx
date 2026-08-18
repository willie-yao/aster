import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import { Link as RouterLink } from "react-router-dom";
import type { RunSummary } from "../types/dashboard";
import { jobRunPath } from "../lib/routes";
import { formatAccessibleDate } from "../lib/utils";
import { dotColorFor } from "../theme";

// Most run dots the overview renders. The backend keeps up to 20 recent runs,
// but the ledger and attention columns reserve a fixed width per dot, so the
// oldest runs are dropped rather than shrinking the targets.
const maxRuns = 12;

// Each dot is a link to its run, so keep targets 24px apart per WCAG 2.5.8.
const desktopCell = 24;

interface SparklineProps {
  runs: RunSummary[];
  jobID: string;
}

export function Sparkline({ runs, jobID }: SparklineProps) {
  const recent = runs.slice(0, maxRuns).reverse();
  const columns = Math.max(recent.length, 1);

  return (
    <Box
      sx={{
        display: "grid",
        // Touch-sized cells that wrap to as many rows as the container allows.
        // auto-fill needs the definite width to resolve its column count.
        gridTemplateColumns: "repeat(auto-fill, 44px)",
        width: "100%",
        alignItems: "center",
        gap: 0,
        // The columns reserve maxRuns * desktopCell, so fixed-width cells keep
        // every run link at its full target size whatever depth is configured.
        "@media (min-width: 1024px)": {
          gridTemplateColumns: `repeat(${columns}, ${desktopCell}px)`,
          width: "auto",
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
                "@media (min-width: 1024px)": { width: desktopCell, height: 28 },
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
