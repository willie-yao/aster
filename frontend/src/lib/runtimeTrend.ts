import type { BuildResult } from "../types/dashboard";

export interface RuntimePoint {
  buildID: string;
  timestamp: string;
  durationSeconds: number;
  passed: boolean;
}

export type RuntimeDirection = "stable" | "increasing" | "decreasing" | "insufficient";

export interface RuntimeSummary {
  points: RuntimePoint[];
  sampleCount: number;
  medianSeconds: number | null;
  p95Seconds: number | null;
  madSeconds: number | null;
  direction: RuntimeDirection;
  changeRatio: number | null;
  latestOutlier: boolean;
}

const trendMinimumSamples = 4;
const outlierMinimumSamples = 5;
const stableChangeRatio = 0.2;
const outlierRatio = 1.5;
const outlierMADs = 3;

function finiteDuration(value: number): boolean {
  return Number.isFinite(value) && value >= 0;
}

function chronological(points: RuntimePoint[]): RuntimePoint[] {
  return points
    .filter((point) => finiteDuration(point.durationSeconds))
    .map((point, index) => ({ point, index }))
    .sort((left, right) => {
      const leftTime = Date.parse(left.point.timestamp);
      const rightTime = Date.parse(right.point.timestamp);
      if (Number.isNaN(leftTime) || Number.isNaN(rightTime)) {
        return left.index - right.index;
      }
      return leftTime - rightTime || left.index - right.index;
    })
    .map(({ point }) => point);
}

export function median(values: number[]): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((left, right) => left - right);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 0
    ? (sorted[middle - 1] + sorted[middle]) / 2
    : sorted[middle];
}

export function nearestRankPercentile(
  values: number[],
  percentile: number,
): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((left, right) => left - right);
  const bounded = Math.min(1, Math.max(0, percentile));
  const rank = Math.max(1, Math.ceil(bounded * sorted.length));
  return sorted[rank - 1];
}

function medianAbsoluteDeviation(values: number[]): number | null {
  const center = median(values);
  if (center === null) return null;
  return median(values.map((value) => Math.abs(value - center)));
}

function trend(values: number[]): {
  direction: RuntimeDirection;
  changeRatio: number | null;
} {
  if (values.length < trendMinimumSamples) {
    return { direction: "insufficient", changeRatio: null };
  }

  const half = Math.floor(values.length / 2);
  const oldestMedian = median(values.slice(0, half));
  const newestMedian = median(values.slice(values.length - half));
  if (oldestMedian === null || newestMedian === null) {
    return { direction: "insufficient", changeRatio: null };
  }
  if (oldestMedian === 0) {
    return newestMedian === 0
      ? { direction: "stable", changeRatio: 0 }
      : { direction: "increasing", changeRatio: null };
  }

  const changeRatio = (newestMedian - oldestMedian) / oldestMedian;
  if (Math.abs(changeRatio) < stableChangeRatio) {
    return { direction: "stable", changeRatio };
  }
  return {
    direction: changeRatio > 0 ? "increasing" : "decreasing",
    changeRatio,
  };
}

function latestIsOutlier(values: number[]): boolean {
  if (values.length < outlierMinimumSamples) return false;
  const latest = values.at(-1);
  const baseline = values.slice(0, -1);
  const baselineMedian = median(baseline);
  const baselineMAD = medianAbsoluteDeviation(baseline);
  if (latest === undefined || baselineMedian === null || baselineMAD === null) {
    return false;
  }
  return (
    latest > baselineMedian * outlierRatio &&
    latest - baselineMedian > baselineMAD * outlierMADs
  );
}

export function summarizeRuntime(points: RuntimePoint[]): RuntimeSummary {
  const ordered = chronological(points);
  const values = ordered.map((point) => point.durationSeconds);
  const direction = trend(values);
  return {
    points: ordered,
    sampleCount: ordered.length,
    medianSeconds: median(values),
    p95Seconds: nearestRankPercentile(values, 0.95),
    madSeconds: medianAbsoluteDeviation(values),
    direction: direction.direction,
    changeRatio: direction.changeRatio,
    latestOutlier: latestIsOutlier(values),
  };
}

export interface RuntimeChartPoint {
  x: number;
  y: number;
  bandX: number;
  bandWidth: number;
}

export interface RuntimeChartLayout {
  path: string;
  points: RuntimeChartPoint[];
}

// Bands split at the midpoints between neighboring samples, so every pointer
// target holds exactly its own sample, tiles the full width, and stays as large
// as the spacing allows however many samples the window holds.
export function runtimeChartLayout(
  values: number[],
  width: number,
  height: number,
  inset: number,
): RuntimeChartLayout {
  if (values.length === 0) return { path: "", points: [] };
  const max = Math.max(...values, 1);
  const plotWidth = width - inset * 2;
  const plotHeight = height - inset * 2;
  const xs = values.map((_, index) =>
    values.length === 1
      ? width / 2
      : inset + (index / (values.length - 1)) * plotWidth,
  );
  const edges = [
    0,
    ...xs.slice(1).map((x, index) => (xs[index] + x) / 2),
    width,
  ];
  const points = values.map((value, index) => ({
    x: xs[index],
    y: inset + plotHeight - (value / max) * plotHeight,
    bandX: edges[index],
    bandWidth: edges[index + 1] - edges[index],
  }));
  return {
    path: points
      .map((point, index) => `${index === 0 ? "M" : "L"} ${point.x} ${point.y}`)
      .join(" "),
    points,
  };
}

export function jobRuntimePoints(runs: BuildResult[]): RuntimePoint[] {
  return chronological(
    runs
      .filter((run) => run.result !== "PENDING" && Boolean(run.finished))
      .map((run) => ({
        buildID: run.build_id,
        timestamp: run.started,
        durationSeconds: run.duration_seconds,
        passed: run.passed,
      })),
  );
}

export function testRuntimePoints(
  runs: BuildResult[],
  testName: string,
): RuntimePoint[] {
  const points: RuntimePoint[] = [];
  for (const run of runs) {
    if (run.result === "PENDING" || !run.finished) continue;
    const testCase = (run.test_cases ?? []).find(
      (candidate) => candidate.name === testName,
    );
    if (!testCase || (testCase.status !== "passed" && testCase.status !== "failed")) {
      continue;
    }
    points.push({
      buildID: run.build_id,
      timestamp: run.started,
      durationSeconds: testCase.duration_seconds,
      passed: testCase.status === "passed",
    });
  }
  return chronological(points);
}
