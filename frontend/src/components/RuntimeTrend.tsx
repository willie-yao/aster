import Box from "@mui/material/Box";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { DetailSectionBand } from "./DetailSectionBand";
import { formatDuration } from "../lib/utils";
import type { RuntimeSummary } from "../lib/runtimeTrend";
import { overviewTypography } from "../theme/overview";

interface RuntimeTrendProps {
  summary: RuntimeSummary;
  subject: string;
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

export function RuntimeTrend({ summary, subject }: RuntimeTrendProps) {
  const width = 720;
  const height = 180;
  const inset = 18;
  const values = summary.points.map((point) => point.durationSeconds);
  const chart = pathFor(values, width, height, inset);
  const max = Math.max(...values, 1);
  const referenceY = (value: number) =>
    inset + (height - inset * 2) - (value / max) * (height - inset * 2);
  const latestIndex = summary.points.length - 1;
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
  const sampleDetails = summary.points
    .map(
      (sample) =>
        `Build ${sample.buildID}, ${sample.passed ? "passed" : "failed"}, ${formatDuration(sample.durationSeconds)}`,
    )
    .join(". ");

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
          <Box
            component="svg"
            viewBox={`0 0 ${width} ${height}`}
            role="img"
            aria-label={`${subject} runtime history. ${summaryText}. ${sampleDetails}`}
            sx={{ width: "100%", height: "auto", minHeight: 140 }}
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
              return (
                <circle
                  key={sample.buildID}
                  cx={point.x}
                  cy={point.y}
                  r={outlier ? 6 : 4}
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
                >
                  <title>
                    {`Build ${sample.buildID}: ${sample.passed ? "passed" : "failed"}, ${formatDuration(sample.durationSeconds)}`}
                  </title>
                </circle>
              );
            })}
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
            <Box component="span">Median: {summary.medianSeconds === null ? "Not available" : formatDuration(summary.medianSeconds)}</Box>
            <Box component="span">p95: {summary.p95Seconds === null ? "Not available" : formatDuration(summary.p95Seconds)}</Box>
            <Box component="span">Direction: {trendLabel(summary)}</Box>
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
