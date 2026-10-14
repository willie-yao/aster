import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { causeBaselineBuildID, causeSuggestedQuestions, patternChatAvailability, patternChatEvidenceBuildIDs, patternChatHasEvidenceBuild } from "../src/lib/patternChat.js";
import type { BuildResult, PatternAnalysis, PatternCausalGroup } from "../src/types/dashboard.js";
import { failedJUnitRun } from "./helpers/failedJUnitRun.js";

const pattern: PatternAnalysis = {
  id: "pattern-id",
  subject: "retry failures",
  generated_at: "2026-07-26T12:00:00Z",
  builds_analyzed: 3,
  systemic: true,
  confidence: "high",
  shared_root_cause: "shared cause",
  shared_builds: ["1", "2"],
  summary: "summary",
};

test("old recurring patterns show an explicit stale state", () => {
  assert.equal(patternChatAvailability(pattern, "job", true, true), "stale");
  assert.equal(patternChatAvailability({ ...pattern, content_hash: "hash" }, "job", true, true), "ready");
});

test("pattern chat still requires identity and current evidence", () => {
  assert.equal(patternChatAvailability({ ...pattern, id: undefined }, "job", true, true), "unavailable");
  assert.equal(patternChatAvailability(pattern, "job", false, true), "unavailable");
  assert.equal(patternChatAvailability({ ...pattern, systemic: false, content_hash: "hash" }, "job", true, true), "ready");
  assert.equal(patternChatAvailability(pattern, "job", true, false), "unavailable");
});

test("causal pattern chat uses repeated-group evidence without requiring every retained build", () => {
  const causal: PatternAnalysis = {
    ...pattern,
    content_hash: "hash",
    recurrence_classification: "mixed_causes",
    causal_groups: [
      { id: "group-a", content_hash: "a", builds: ["4", "3"], root_cause: "cause a", confidence: "high" },
      { id: "singleton", content_hash: "s", builds: ["2"], root_cause: "outlier", confidence: "low" },
      { id: "group-b", content_hash: "b", builds: ["1", "0"], root_cause: "cause b", confidence: "medium" },
    ],
    unclassified_builds: ["unknown"],
  };
  assert.deepEqual(patternChatEvidenceBuildIDs(causal), ["4", "3", "1", "0"]);
  assert.equal(patternChatHasEvidenceBuild(causal, ["4"], false), true);
  assert.equal(patternChatHasEvidenceBuild(causal, ["2", "unknown"], false), false);
  assert.equal(patternChatHasEvidenceBuild(causal, ["4"], true), false);
  assert.equal(patternChatHasEvidenceBuild(causal, ["4", "3", "1", "0"], true), true);
  assert.equal(patternChatAvailability(causal, "job", true, true), "ready");
});

test("legacy pattern chat keeps shared-build evidence selection", () => {
  assert.deepEqual(patternChatEvidenceBuildIDs(pattern), ["1", "2"]);
});

test("pattern chat suggested questions remain useful for causal groups", () => {
  const source = readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8");
  assert.match(source, /How do the failures differ across the identified causes/);
  assert.match(source, /Which builds support each cause/);
  assert.match(source, /unclassified or outliers/);
  assert.doesNotMatch(source, /What would disprove this shared root cause/);
});

test("cause chat uses multi-build evidence without inheriting pattern fix authority", () => {
  const source = readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8");
  assert.match(source, /const causeScope = analysisRef\.scope === "cause"/);
  assert.match(source, /const multiBuildScope = patternScope \|\| causeScope/);
  assert.match(source, /exactFailureAnalysis = !multiBuildScope/);
  assert.match(source, /fixEligible=\{exactFixEnabled \|\| \(patternScope && Boolean\(features\.chat_fix\) && Boolean\(fixPatterns\.length\)\)/);
  assert.match(source, /chatTitle = causeScope \? "Investigate cause"/);
  assert.ok(causeSuggestedQuestions().includes("Has this cause been resolved in the latest completed run?"));
  assert.ok(causeSuggestedQuestions().includes("What evidence supports this cause across its builds?"));
  assert.ok(causeSuggestedQuestions().includes("What concrete change follows from this cause?"));
  assert.match(source, /Interrogate this cause across failed and later runs/);
});

test("cause chat keeps a bound session baseline authoritative", () => {
  const question = "What changed since the last passing run?";
  assert.equal(causeSuggestedQuestions().includes(question), false);
  assert.equal(causeSuggestedQuestions("").includes(question), false);
  assert.equal(causeSuggestedQuestions("100").includes(question), true);
  assert.equal(causeSuggestedQuestions("100", null).includes(question), true);
  assert.equal(causeSuggestedQuestions("100", {}).includes(question), false);
  assert.equal(causeSuggestedQuestions("100", { baseline_build_id: "" }).includes(question), false);
  assert.equal(causeSuggestedQuestions(undefined, { baseline_build_id: "99" }).includes(question), true);
  const source = readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8");
  const banner = readFileSync(resolve(process.cwd(), "src/components/PatternBanner.tsx"), "utf8");
  const nextStep = readFileSync(resolve(process.cwd(), "src/components/CausalGroupNextStep.tsx"), "utf8");
  assert.match(source, /causeScope \? causeSuggestedQuestions\(baselineBuildID, session\)/);
  assert.match(banner, /baselineBuildID: causeBaselineBuildID\(group, runs\)/);
  assert.match(nextStep, /baselineBuildID=\{chat\.baselineBuildID\}/);
});

const cause: PatternCausalGroup = {
  builds: ["5", " 3 "],
  root_cause: "cause",
  confidence: "high",
};

function passingRun(buildID: string): BuildResult {
  return {
    ...failedJUnitRun(buildID, "TestCluster"),
    passed: true,
    result: "SUCCESS",
    tests_failed: 0,
    junit_urls: ["https://gcsweb.example/artifacts/junit.xml"],
  };
}

test("cause chat offers the comparison question before a session exists", () => {
  const runs = [
    passingRun("4"),
    passingRun("1"),
    failedJUnitRun("5", "TestCluster"),
    passingRun("2"),
    failedJUnitRun("3", "TestCluster"),
  ];
  assert.equal(causeBaselineBuildID(cause, runs), "2");
  assert.ok(causeSuggestedQuestions(causeBaselineBuildID(cause, runs), null)
    .includes("What changed since the last passing run?"));
  assert.deepEqual(runs.map((run) => run.build_id), ["4", "1", "5", "2", "3"]);
});

test("passing runs with truncated dashboard JUnit still enable the comparison question", () => {
  const runs = [
    failedJUnitRun("3", "TestCluster"),
    { ...passingRun("2"), junit_truncated: true, junit_complete: false, test_cases: [] },
  ];
  assert.ok(causeSuggestedQuestions(causeBaselineBuildID(cause, runs))
    .includes("What changed since the last passing run?"));
});

test("cause chat hides the comparison question without an older qualifying run", () => {
  const member = failedJUnitRun("3", "TestCluster");
  for (const runs of [
    [],
    [passingRun("2")],
    [member],
    [member, passingRun("4")],
    [member, failedJUnitRun("2", "TestCluster")],
    [member, { ...passingRun("2"), passed: false }],
    [member, { ...passingRun("2"), result: "FAILURE" }],
    [member, { ...passingRun("2"), tests_failed: 1 }],
    [member, { ...passingRun("2"), junit_urls: [] }],
    [member, { ...passingRun("2"), junit_urls: undefined }],
  ]) {
    assert.equal(causeSuggestedQuestions(causeBaselineBuildID(cause, runs))
      .includes("What changed since the last passing run?"), false);
  }
});

test("baseline selection skips ineligible runs and orders by time before build ID", () => {
  const runs = [
    { ...passingRun("100"), started: "2026-08-16T00:00:00Z" },
    { ...passingRun("2"), started: "2026-08-17T00:00:00Z" },
    { ...passingRun("1"), junit_urls: [], started: "2026-08-17T12:00:00Z" },
    failedJUnitRun("3", "TestCluster"),
  ];
  assert.equal(causeBaselineBuildID(cause, runs), "2");
});

test("baseline build ID ordering preserves Prow integer precision", () => {
  const group = { ...cause, builds: ["2107583444329435136"] };
  const runs = [
    passingRun("2106473116585693183"),
    failedJUnitRun("2107583444329435136", "TestCluster"),
    passingRun("2106473116585693184"),
  ];
  assert.equal(causeBaselineBuildID(group, runs), "2106473116585693184");
});

test("baseline ordering falls back to descending text for nonnumeric IDs", () => {
  const group = { ...cause, builds: ["z"] };
  assert.equal(causeBaselineBuildID(group, [
    passingRun("a"), failedJUnitRun("z", "TestCluster"), passingRun("b"),
  ]), "b");
});
