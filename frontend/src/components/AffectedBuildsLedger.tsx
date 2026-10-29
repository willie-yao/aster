import { useMemo, useState } from "react";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { accentLabelSx } from "../theme";
import { overviewTypography, touchTargetSx } from "../theme/overview";
import { compareBuildIDs } from "../lib/patternFixGuidance";
import { jobRunPath } from "../lib/routes";
import { formatDuration } from "../lib/utils";
import type { BuildResult } from "../types/dashboard";

// Rows shown before the ledger folds; the newest builds are the ones to open.
const visibleRows = 4;

// Days are local like the run history beside the ledger, and one formatter
// keeps the visible and spoken labels on the same day.
function dayLabel(started: string, year = false): string | null {
  const date = new Date(started);
  return Number.isFinite(date.getTime())
    ? date.toLocaleDateString("en-US", { month: "short", day: "numeric", ...(year && { year: "numeric" }) })
    : null;
}

/**
 * Builds a cause or pattern was correlated from, newest first, one aligned row
 * each with when it ran and how long it took. Builds older than the loaded run
 * window keep their exact ID but have no date or duration to show.
 */
export function AffectedBuildsLedger({ builds, runs, jobID }: { builds: string[]; runs: BuildResult[]; jobID?: string }) {
  const [expanded, setExpanded] = useState(false);
  const affected = useMemo(() => {
    const byID = new Map(runs.map((run) => [run.build_id, run]));
    return [...new Set(builds)]
      .sort((a, b) => compareBuildIDs(b, a))
      .map((buildID) => ({ buildID, run: byID.get(buildID) }));
  }, [builds, runs]);
  const shown = expanded ? affected : affected.slice(0, visibleRows);

  return (
    <Box
      component="ul"
      sx={{
        m: 0,
        p: 0,
        listStyle: "none",
        border: "1px solid",
        borderColor: "divider",
        borderRadius: 1,
        overflow: "hidden",
        "& > li + li": { borderTop: "1px solid", borderColor: "divider" },
      }}
    >
      {shown.map(({ buildID, run }) => {
        const day = run ? dayLabel(run.started) : null;
        const description = run
          ? `${day ? `, ${dayLabel(run.started, true)}` : ""}, ran ${formatDuration(run.duration_seconds)}`
          : ", before the loaded runs";
        return (
          <li key={buildID}>
            <Link
              component={RouterLink}
              to={jobID ? jobRunPath(jobID, buildID) : "#"}
              underline="none"
              aria-label={`Open affected build ${buildID}${description}`}
              sx={{
                display: "grid",
                // Phones drop the duration so the full build ID still fits.
                gridTemplateColumns: { xs: "56px minmax(0, 1fr) 16px", sm: "64px minmax(0, 1fr) 72px 16px" },
                alignItems: "center",
                columnGap: 1.5,
                minHeight: 44,
                px: 1.5,
                color: "text.primary",
                "&:hover": { bgcolor: "surface.containerHigh" },
                "&:hover .ledger-arrow": { color: "primary.main", transform: "translateX(2px)" },
                "&:focus-visible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 },
              }}
            >
              <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}>
                {day ?? "Earlier"}
              </Typography>
              <Typography
                component="span"
                title={buildID}
                sx={(theme) => ({
                  ...overviewTypography.data,
                  ...accentLabelSx(theme, "primary"),
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                  whiteSpace: "nowrap",
                })}
              >
                {buildID}
              </Typography>
              <Typography
                component="span"
                sx={{ ...overviewTypography.data, color: "text.secondary", textAlign: "right", display: { xs: "none", sm: "block" } }}
              >
                {run ? formatDuration(run.duration_seconds) : ""}
              </Typography>
              <Box
                component="span"
                className="ledger-arrow"
                aria-hidden="true"
                sx={{
                  color: "text.secondary",
                  transition: "transform 140ms ease, color 140ms ease",
                  "@media (prefers-reduced-motion: reduce)": { transition: "none" },
                }}
              >
                →
              </Box>
            </Link>
          </li>
        );
      })}
      {affected.length > visibleRows && (
        <li>
          <ButtonBase
            onClick={() => setExpanded((open) => !open)}
            aria-expanded={expanded}
            sx={{
              ...touchTargetSx,
              width: "100%",
              minHeight: 40,
              justifyContent: "flex-start",
              px: 1.5,
              ...overviewTypography.description,
              fontWeight: 650,
              color: "primary.main",
              "&:hover": { bgcolor: "surface.containerHigh" },
              "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: -2 },
            }}
          >
            {expanded ? "Show fewer" : `Show all ${affected.length}`}
          </ButtonBase>
        </li>
      )}
    </Box>
  );
}
