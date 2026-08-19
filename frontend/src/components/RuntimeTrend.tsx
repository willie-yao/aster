import Box from "@mui/material/Box";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useEffect, useRef } from "react";
import { Link as RouterLink } from "react-router-dom";
import { DetailSectionBand } from "./DetailSectionBand";
import { formatDuration } from "../lib/utils";
import type { RuntimeSummary } from "../lib/runtimeTrend";
import { overviewTypography } from "../theme/overview";

interface RuntimeTrendProps {
  summary: RuntimeSummary;
  subject: string;
  // Destination for one sample. The job page keeps the reader on the job and
  // selects the run; the test page keeps them on the test.
  runHref: (buildID: string) => string;
}

function trendLabel(summary: RuntimeSummary): string {
  if (summary.direction === "insufficient") return "Need at least 4 samples";
  if (summary.changeRatio === null) {
    return summary.direction === "increasing"
      ? "Increasing from a zero baseline"
      : "Stable";
  }
  const percent = Math.round(Math.abs(summary.changeRatio) * 100);
  if (summary.direction === "stable") return `Stable (${percent}% change)`;
  return `${summary.direction === "increasing" ? "Increasing" : "Decreasing"} ${percent}%`;
}

function pathFor(
  values: number[],
  width: number,
  height: number,
  inset: number,
): { path: string; points: Array<{ x: number; y: number }> } {
  if (values.length === 0) return { path: "", points: [] };
  const max = Math.max(...values, 1);
  const plotWidth = width - inset * 2;
  const plotHeight = height - inset * 2;
  const points = values.map((value, index) => ({
    x:
      values.length === 1
        ? width / 2
        : inset + (index / (values.length - 1)) * plotWidth,
    y: inset + plotHeight - (value / max) * plotHeight,
  }));
  return {
    path: points
      .map((point, index) => `${index === 0 ? "M" : "L"} ${point.x} ${point.y}`)
      .join(" "),
    points,
  };
}

export function RuntimeTrend({ summary, subject, runHref }: RuntimeTrendProps) {
  const height = 180;
  const inset = 18;
  const values = summary.points.map((point) => point.durationSeconds);
  // Two different widths. The drawing width sets the viewBox, so a long history
  // widens the coordinate space instead of stretching a fixed one, which would
  // scale the chart taller as it grows. The CSS reserve is what the scroll
  // container honours, and it stays below the drawing width at short histories
  // so the common case still fills its rail without scrolling.
  const width = Math.max(720, values.length * 32);
  const minChartWidth = Math.max(320, values.length * 32);
  const chart = pathFor(values, width, height, inset);
  const max = Math.max(...values, 1);
  const referenceY = (value: number) =>
    inset + (height - inset * 2) - (value / max) * (height - inset * 2);
  // The run count is consumer-tuned, so the target scales with the spacing and
  // can never overlap its neighbours.
  const spacing =
    values.length > 1 ? (width - inset * 2) / (values.length - 1) : width;
  const hitRadius = Math.min(17, spacing * 0.45);
  const latestIndex = summary.points.length - 1;
  // Points run oldest to newest, so a scrolling history would otherwise open on
  // the oldest samples and hide the latest run and any outlier.
  const scroller = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const node = scroller.current;
    if (node) node.scrollLeft = node.scrollWidth;
  }, [latestIndex, minChartWidth]);
  const summaryText = [
    `${summary.sampleCount} ${summary.sampleCount === 1 ? "sample" : "samples"}`,
    summary.medianSeconds === null
      ? null
      : `median ${formatDuration(summary.medianSeconds)}`,
    summary.p95Seconds === null ? null : `p95 ${formatDuration(summary.p95Seconds)}`,
    trendLabel(summary),
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <Box
      component="section"
      aria-label={`${subject} runtime trend`}
      sx={{
        minWidth: 0,
        bgcolor: "surface.container",
        borderBottom: "1px solid",
        borderColor: "divider",
      }}
    >
      <DetailSectionBand title="Runtime trend" metadata={summaryText} />
      {summary.points.length === 0 ? (
        <Typography
          color="textSecondary"
          sx={{ px: 1.5, py: 2, ...overviewTypography.secondaryBody }}
        >
          No completed runtime samples are available in the current build window.
        </Typography>
      ) : (
        <Stack spacing={1} sx={{ px: 1.5, py: 1.5 }}>
          <Box ref={scroller} sx={{ width: "100%", minWidth: 0, overflowX: "auto", overflowY: "hidden" }}>
          <Box
            component="svg"
            viewBox={`0 0 ${width} ${height}`}
            // Deliberately not an image role: that would make the chart atomic
            // and hide the per-run links inside it from assistive technology.
            aria-label={`${subject} runtime history. ${summaryText}`}
            sx={{ width: "100%", minWidth: minChartWidth, height: "auto", minHeight: 140 }}
          >
            <title>{`${subject} runtime history. ${summaryText}`}</title>
            {summary.medianSeconds !== null && (
              <line
                x1={inset}
                x2={width - inset}
                y1={referenceY(summary.medianSeconds)}
                y2={referenceY(summary.medianSeconds)}
                stroke="var(--mui-palette-text-secondary)"
                strokeDasharray="7 6"
                opacity="0.55"
              />
            )}
            {summary.p95Seconds !== null && (
              <line
                x1={inset}
                x2={width - inset}
                y1={referenceY(summary.p95Seconds)}
                y2={referenceY(summary.p95Seconds)}
                stroke="var(--mui-palette-warning-main)"
                strokeDasharray="3 5"
                opacity="0.55"
              />
            )}
            <path
              d={chart.path}
              fill="none"
              stroke="var(--mui-palette-primary-main)"
              strokeWidth="3"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            {chart.points.map((point, index) => {
              const sample = summary.points[index];
              const outlier = index === latestIndex && summary.latestOutlier;
              const label = `Build ${sample.buildID}: ${sample.passed ? "passed" : "failed"}, ${formatDuration(sample.durationSeconds)}`;
              return (
                <Tooltip key={sample.buildID} title={label}>
                  <Box
                    component={RouterLink}
                    to={runHref(sample.buildID)}
                    aria-label={`Open run ${sample.buildID}, ${sample.passed ? "passed" : "failed"}, ${formatDuration(sample.durationSeconds)}`}
                    sx={{
                      cursor: "pointer",
                      "&:focus-visible": {
                        outline: "2px solid",
                        outlineColor: "primary.main",
                      },
                    }}
                  >
                    {/* A transparent target keeps the pointer and keyboard hit
                        area usable without enlarging the plotted dot. The chart
                        scales with its container, so this clears 24 CSS px at
                        common widths and stays well inside the spacing between
                        adjacent runs on narrow ones. */}
                    <circle cx={point.x} cy={point.y} r={hitRadius} fill="transparent" />
                    <circle
                      cx={point.x}
                      cy={point.y}
                      r={outlier ? 6 : 4}
                      // The transparent target above owns the hit area, so a
                      // large outlier marker cannot reach past it into the
                      // neighbouring run's target.
                      pointerEvents="none"
                      fill={
                        sample.passed
                          ? "var(--mui-palette-success-main)"
                          : "var(--mui-palette-error-main)"
                      }
                      stroke={
                        outlier
                          ? "var(--mui-palette-warning-main)"
                          : "var(--mui-palette-background-default)"
                      }
                      strokeWidth={outlier ? 4 : 2}
                    />
                  </Box>
                </Tooltip>
              );
            })}
          </Box>
          </Box>
          <Box
            sx={{
              display: "flex",
              alignItems: "center",
              gap: 2,
              flexWrap: "wrap",
              color: "text.secondary",
              ...overviewTypography.description,
            }}
          >
            {/* The band above states median, p95, and direction, so this row
                explains what the reference lines mean instead of restating
                their values. Each swatch repeats its line's dash pattern, so
                the mapping does not depend on telling the colours apart. */}
            <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: 0.75 }}>
              <Box component="svg" aria-hidden width={20} height={8} viewBox="0 0 20 8">
                <line
                  x1={0}
                  x2={20}
                  y1={4}
                  y2={4}
                  stroke="var(--mui-palette-text-secondary)"
                  strokeDasharray="7 6"
                  strokeWidth={2}
                  opacity="0.55"
                />
              </Box>
              Median
            </Box>
            <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: 0.75 }}>
              <Box component="svg" aria-hidden width={20} height={8} viewBox="0 0 20 8">
                <line
                  x1={0}
                  x2={20}
                  y1={4}
                  y2={4}
                  stroke="var(--mui-palette-warning-main)"
                  strokeDasharray="3 5"
                  strokeWidth={2}
                  opacity="0.55"
                />
              </Box>
              p95
            </Box>
            <Box component="span">Select a point to open that run</Box>
          </Box>
          {summary.latestOutlier && (
            <Typography color="warning" sx={overviewTypography.secondaryBody}>
              The latest runtime is unusually high compared with this subject&apos;s
              recent history. This is an observed outlier, not proof of a regression&apos;s
              cause.
            </Typography>
          )}
        </Stack>
      )}
    </Box>
  );
}
