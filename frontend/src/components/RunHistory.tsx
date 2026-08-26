import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { BuildResult } from "../types/dashboard";
import { dotColorFor } from "../theme";
import { formatAccessibleDate, shortDate } from "../lib/utils";
import { overviewTypography } from "../theme/overview";
import { DetailSectionBand } from "./DetailSectionBand";

interface RunHistoryProps {
  runs: BuildResult[];
  selectedBuildId?: string;
  onSelect: (buildId: string) => void;
  metadata?: string;
  title?: string;
  colorFn?: (run: BuildResult) => string;
  resultLabelFn?: (run: BuildResult) => string;
}

function defaultResultLabel(run: BuildResult): string {
  if (run.result === "PENDING") return "Running";
  return run.passed ? "Passed" : "Failed";
}

export function RunHistory({
  runs,
  selectedBuildId,
  onSelect,
  metadata,
  title = "Run history",
  colorFn,
  resultLabelFn = defaultResultLabel,
}: RunHistoryProps) {
  const theme = useTheme();
  const scrollerRef = useRef<HTMLDivElement | null>(null);
  const [overflowing, setOverflowing] = useState(false);
  const sorted = [...runs].sort(
    (a, b) => new Date(a.started).getTime() - new Date(b.started).getTime(),
  );
  const selectedRun = sorted.find((run) => run.build_id === selectedBuildId);

  useEffect(() => {
    const scroller = scrollerRef.current;
    if (!scroller) return;
    const update = () => setOverflowing(scroller.scrollWidth > scroller.clientWidth + 1);
    update();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(update);
    observer?.observe(scroller);
    window.addEventListener("resize", update);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", update);
    };
  }, [sorted.length]);

  useLayoutEffect(() => {
    const scroller = scrollerRef.current;
    const selected = scroller?.querySelector<HTMLElement>('[data-selected="true"]');
    if (!scroller || !selected) return;
    scroller.scrollLeft = Math.max(
      0,
      selected.offsetLeft - (scroller.clientWidth - selected.offsetWidth) / 2,
    );
  }, [selectedBuildId, sorted.length]);

  const bandMetadata = (
    <>
      <Box component="span" sx={{ display: { xs: "none", sm: "inline" } }}>
        {metadata}
      </Box>
      <Box component="span" sx={{ display: { xs: "inline", sm: "none" } }}>
        {overflowing ? "Scroll runs ↔" : metadata}
      </Box>
    </>
  );

  return (
    <Box component="section" sx={{ minWidth: 0, maxWidth: "100%" }}>
      <DetailSectionBand title={title} metadata={bandMetadata} />
      <Box
        ref={scrollerRef}
        aria-label={title}
        sx={{
          width: "100%",
          minWidth: 0,
          maxWidth: "100%",
          overflowX: "auto",
          overflowY: "hidden",
          bgcolor: "surface.container",
          borderBottom: "1px solid",
          borderColor: "divider",
          scrollbarWidth: "thin",
        }}
      >
        <Box
          sx={{
            width: "max-content",
            minWidth: "100%",
            display: "flex",
            alignItems: "flex-start",
            gap: { xs: 1, sm: 0.5 },
            px: 1.5,
            py: 1.25,
          }}
        >
          {sorted.map((run, index) => {
            const isSelected = run.build_id === selectedBuildId;
            const showDate = sorted.length <= 10 || index % 5 === 0 || index === sorted.length - 1;
            const result = resultLabelFn(run);
            const color = colorFn ? colorFn(run) : dotColorFor(theme, run.passed, run.result);
            const tooltip = `#${run.build_id} · ${result} · ${formatAccessibleDate(run.started)}`;

            return (
              <Box
                key={run.build_id}
                data-selected={isSelected ? "true" : undefined}
                sx={{
                  width: { xs: 44, sm: 32 },
                  flex: { xs: "0 0 44px", sm: "0 0 32px" },
                  display: "flex",
                  flexDirection: "column",
                  alignItems: "center",
                }}
              >
                <Tooltip title={tooltip}>
                  <ButtonBase
                    type="button"
                    onClick={() => onSelect(run.build_id)}
                    aria-label={tooltip}
                    aria-pressed={isSelected}
                    sx={{
                      width: { xs: 44, sm: 32 },
                      height: { xs: 44, sm: 32 },
                      borderRadius: "2px",
                      bgcolor: color,
                      outline: "2px solid",
                      outlineColor: isSelected ? "primary.main" : "transparent",
                      outlineOffset: 2,
                      transition: (currentTheme) =>
                        currentTheme.transitions.create(["filter", "outline-color", "box-shadow"], {
                          duration: currentTheme.transitions.duration.shortest,
                        }),
                      "&:hover": { filter: "brightness(1.12)" },
                      "&.Mui-focusVisible": {
                        outlineColor: "primary.main",
                        boxShadow: "0 0 0 5px var(--mui-palette-background-default), 0 0 0 7px var(--mui-palette-text-primary)",
                      },
                    }}
                  />
                </Tooltip>
                <Typography
                  component="span"
                  color="textSecondary"
                  sx={{
                    mt: 0.75,
                    visibility: showDate ? "visible" : "hidden",
                    fontSize: "0.6875rem",
                    lineHeight: "14px",
                    fontFamily: overviewTypography.data.fontFamily,
                  }}
                >
                  {shortDate(run.started)}
                </Typography>
              </Box>
            );
          })}
        </Box>
      </Box>
      {selectedRun && (
        <Typography
          component="div"
          color="textSecondary"
          sx={{
            px: 1.5,
            py: 0.75,
            bgcolor: "surface.container",
            borderBottom: "1px solid",
            borderColor: "divider",
            ...overviewTypography.data,
          }}
        >
          Selected #{selectedRun.build_id} · {resultLabelFn(selectedRun)}
        </Typography>
      )}
    </Box>
  );
}
