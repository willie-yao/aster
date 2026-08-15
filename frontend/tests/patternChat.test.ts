import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { patternChatAvailability, patternChatEvidenceBuildIDs, patternChatHasEvidenceBuild } from "../src/lib/patternChat.js";
import type { PatternAnalysis } from "../src/types/dashboard.js";

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
  assert.equal(patternChatAvailability({ ...pattern, systemic: false }, "job", true, true), "unavailable");
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
