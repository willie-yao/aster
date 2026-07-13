import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";
import type { BuildResult } from "../types/dashboard";
import { dotColorFor } from "../theme";
import { shortDate } from "../lib/utils";

interface RunTimelineProps {
  runs: BuildResult[];
  selectedBuildId?: string;
  onSelect: (buildId: string) => void;
  /** CSS color string per run. Defaults to pass/fail from run.passed. */
  colorFn?: (run: BuildResult) => string;
  /** Custom tooltip per run. */
  tooltipFn?: (run: BuildResult) => string;
}

export function RunTimeline({
  runs,
  selectedBuildId,
  onSelect,
  colorFn,
  tooltipFn,
}: RunTimelineProps) {
  const theme = useTheme();
  const sorted = [...runs].sort(
    (a, b) => new Date(a.started).getTime() - new Date(b.started).getTime(),
  );

  return (
    <Box sx={{ overflowX: "auto" }}>
      <Box sx={{ display: "flex", alignItems: "flex-start", gap: 1, p: 0.5 }}>
        {sorted.map((run, i) => {
          const isSelected = run.build_id === selectedBuildId;
          const showDate = sorted.length <= 10 || i % 5 === 0 || i === sorted.length - 1;
          const color = colorFn ? colorFn(run) : dotColorFor(theme, run.passed, run.result);
          const tooltip = tooltipFn ? tooltipFn(run) : `#${run.build_id} — ${run.passed ? "Passed" : "Failed"}`;

          return (
            <Box
              key={run.build_id}
              sx={{ display: "flex", flexDirection: "column", alignItems: "center" }}
            >
              <ButtonBase
                type="button"
                onClick={() => onSelect(run.build_id)}
                title={tooltip}
                aria-label={tooltip}
                sx={{
                  width: 40,
                  height: 24,
                  borderRadius: 0.5,
                  bgcolor: color,
                  transition: (t) =>
                    t.transitions.create(["filter", "box-shadow", "outline-color"], {
                      duration: t.transitions.duration.shortest,
                    }),
                  outline: isSelected ? "2px solid" : "2px solid transparent",
                  outlineColor: isSelected ? "primary.main" : "transparent",
                  outlineOffset: 2,
                  boxShadow: isSelected
                    ? (t) => `0 0 0 4px ${(t.vars ?? t).palette.surface.main}`
                    : "none",
                  "&:hover": {
                    filter: "brightness(1.18)",
                  },
                  "&.Mui-focusVisible": {
                    outlineColor: "primary.main",
                    boxShadow: (t) => `0 0 0 4px ${(t.vars ?? t).palette.surface.main}`,
                  },
                }}
              />
              <Typography
                variant="caption"
                component="span"
                sx={{
                  mt: 0.75,
                  fontSize: "0.5625rem",
                  color: "text.secondary",
                  visibility: showDate ? "visible" : "hidden",
                  lineHeight: 1.2,
                }}
              >
                {shortDate(run.started)}
              </Typography>
            </Box>
          );
        })}
      </Box>
    </Box>
  );
}
