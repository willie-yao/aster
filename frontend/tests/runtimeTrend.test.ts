import assert from "node:assert/strict";
import { test } from "node:test";
import {
  jobRuntimePoints,
  median,
  nearestRankPercentile,
  summarizeRuntime,
  testRuntimePoints,
  type RuntimePoint,
} from "../src/lib/runtimeTrend.js";
import type { BuildResult, TestCase } from "../src/types/dashboard.js";

function point(
  durationSeconds: number,
  index: number,
): RuntimePoint {
  return {
    buildID: String(index),
    timestamp: `2026-08-${String(index + 1).padStart(2, "0")}T00:00:00Z`,
    durationSeconds,
    passed: true,
  };
}

function build(
  buildID: string,
  durationSeconds: number,
  result = "SUCCESS",
  testCases: TestCase[] = [],
): BuildResult {
  return {
    build_id: buildID,
    job_name: "job",
    started: `2026-08-${buildID.padStart(2, "0")}T00:00:00Z`,
    finished: result === "PENDING" ? "" : `2026-08-${buildID.padStart(2, "0")}T01:00:00Z`,
    passed: result === "SUCCESS",
    result,
    duration_seconds: durationSeconds,
    commit: "0123456789abcdef0123456789abcdef01234567",
    prow_url: "https://prow.example/build",
    web_url: "https://storage.example/build",
    build_log_url: "https://storage.example/build-log.txt",
    test_cases: testCases,
    tests_total: testCases.length,
    tests_passed: testCases.filter((item) => item.status === "passed").length,
    tests_failed: testCases.filter((item) => item.status === "failed").length,
    tests_skipped: testCases.filter((item) => item.status === "skipped").length,
  };
}

function testCase(
  name: string,
  status: TestCase["status"],
  durationSeconds: number,
): TestCase {
  return { name, status, duration_seconds: durationSeconds };
}

test("runtime statistics use median and nearest-rank p95", () => {
  assert.equal(median([4, 1, 3]), 3);
  assert.equal(median([4, 1, 3, 2]), 2.5);
  assert.equal(nearestRankPercentile([1, 2, 3, 4, 5], 0.95), 5);
  assert.equal(
    nearestRankPercentile(Array.from({ length: 20 }, (_, index) => index + 1), 0.95),
    19,
  );
});

test("runtime direction compares oldest and newest half medians", () => {
  const stable = summarizeRuntime([100, 110, 115, 120].map(point));
  assert.equal(stable.direction, "stable");

  const increasing = summarizeRuntime([100, 100, 140, 140].map(point));
  assert.equal(increasing.direction, "increasing");
  assert.equal(increasing.changeRatio, 0.4);

  const decreasing = summarizeRuntime([140, 140, 100, 100].map(point));
  assert.equal(decreasing.direction, "decreasing");
  assert.ok((decreasing.changeRatio ?? 0) < -0.28);

  assert.equal(
    summarizeRuntime([100, 120, 140].map(point)).direction,
    "insufficient",
  );
});

test("latest runtime outlier requires both ratio and MAD gates", () => {
  assert.equal(
    summarizeRuntime([100, 101, 99, 100, 180].map(point)).latestOutlier,
    true,
  );
  assert.equal(
    summarizeRuntime([100, 100, 100, 100, 149].map(point)).latestOutlier,
    false,
  );
  assert.equal(
    summarizeRuntime([10, 20, 30, 40, 50].map(point)).latestOutlier,
    false,
  );
  assert.equal(
    summarizeRuntime([100, 100, 100, 100, 151].map(point)).latestOutlier,
    true,
  );
  assert.equal(
    summarizeRuntime([100, 100, 100, 151].map(point)).latestOutlier,
    false,
  );
});

test("job runtime points exclude pending, incomplete, and invalid runs", () => {
  const incomplete = build("3", 30);
  incomplete.finished = "";
  const negative = build("4", -1);
  const nonFinite = build("5", Number.NaN);
  const points = jobRuntimePoints([
    build("2", 20, "PENDING"),
    incomplete,
    negative,
    nonFinite,
    build("1", 10),
  ]);
  assert.deepEqual(points.map((item) => item.buildID), ["1"]);
});

test("test runtime points include passed and failed but not skipped or absent", () => {
  const unfinished = build("5", 50, "SUCCESS", [
    testCase("target", "passed", 50),
  ]);
  unfinished.finished = "";
  const points = testRuntimePoints(
    [
      build("6", 60, "PENDING", [testCase("target", "passed", 60)]),
      unfinished,
      build("4", 40, "SUCCESS", [testCase("target", "skipped", 40)]),
      build("3", 30, "FAILURE", [testCase("target", "failed", 30)]),
      build("2", 20, "SUCCESS", [testCase("other", "passed", 20)]),
      build("1", 10, "SUCCESS", [testCase("target", "passed", 10)]),
    ],
    "target",
  );
  assert.deepEqual(
    points.map((item) => [item.buildID, item.durationSeconds, item.passed]),
    [
      ["1", 10, true],
      ["3", 30, false],
    ],
  );
});
