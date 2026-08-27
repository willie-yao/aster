import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import { useRef, useState, type KeyboardEvent } from "react";
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

// Dot size within that cell. Sized to read as a near-continuous pass/fail
// ribbon, matching the run history strip on the job and test detail pages.
const dotSize = 18;

interface SparklineProps {
  runs: RunSummary[];
  jobID: string;
}

export function Sparkline({ runs, jobID }: SparklineProps) {
  const recent = runs.slice(0, maxRuns).reverse();
  const columns = Math.max(recent.length, 1);
  const links = useRef<(HTMLAnchorElement | null)[]>([]);
  const [focused, setFocused] = useState(0);
  // The strip is one tab stop that arrow keys move within. A ledger of dozens
  // of jobs would otherwise put hundreds of run links in the overview tab
  // order. Clamped here so a shorter history cannot strand the tabbable run.
  const tabbable = Math.min(focused, recent.length - 1);

  const moveFocus = (event: KeyboardEvent<HTMLDivElement>) => {
    const last = recent.length - 1;
    // Alt, Control, and Meta carry browser shortcuts such as Back and
    // scroll-to-end, so the strip leaves those to the browser.
    if (last < 0 || event.altKey || event.ctrlKey || event.metaKey) return;
    // Each link records its own position on focus, so moving focus is enough.
    if (event.key === "ArrowRight") links.current[Math.min(last, tabbable + 1)]?.focus();
    else if (event.key === "ArrowLeft") links.current[Math.max(0, tabbable - 1)]?.focus();
    else if (event.key === "Home") links.current[0]?.focus();
    else if (event.key === "End") links.current[last]?.focus();
    else return;
    event.preventDefault();
  };

  return (
    <Box
      role="group"
      aria-label="Recent runs"
      onKeyDown={moveFocus}
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
      {recent.map((run, index) => {
        const label = run.result === "PENDING" ? "Running" : run.passed ? "Passed" : "Failed";
        const date = formatAccessibleDate(run.timestamp);
        const context = `Run ${run.build_id}, ${label.toLowerCase()}, ${date}`;
        return (
          <Tooltip key={run.build_id} title={`#${run.build_id} - ${label} - ${date}`}>
            <Box
              component={RouterLink}
              ref={(node: HTMLAnchorElement | null) => {
                links.current[index] = node;
              }}
              to={jobRunPath(jobID, run.build_id)}
              aria-label={context}
              tabIndex={index === tabbable ? 0 : -1}
              onFocus={() => setFocused(index)}
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
                  width: dotSize,
                  height: dotSize,
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
