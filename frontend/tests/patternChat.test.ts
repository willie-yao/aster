import assert from "node:assert/strict";
import { test } from "node:test";

import { patternChatAvailability } from "../src/lib/patternChat.js";
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
