import assert from "node:assert/strict";
import test from "node:test";
import {
  actionEligibilityTitle,
  buildActionEligibilityHint,
  eligibilityForState,
  patternActionEligibilityHint,
} from "../src/lib/actionEligibility.js";

const actionableTarget = { intent: "add_symbol" as const, path: "main.go", symbol: "MissingHelper" };

test("pattern action eligibility handles deterministic blocked states", () => {
  assert.equal(patternActionEligibilityHint(undefined)?.state, "more_evidence_required");
  assert.equal(patternActionEligibilityHint([{ intent: "investigate" }])?.state, "investigation_required");
  assert.equal(patternActionEligibilityHint([{ intent: "add_symbol", path: "main.go" }])?.state, "more_evidence_required");
  assert.equal(patternActionEligibilityHint([actionableTarget]), null);
  const existing = patternActionEligibilityHint([actionableTarget], "open");
  assert.equal(existing?.state, "already_present");
  assert.match(existing?.reason ?? "", /attempt already exists/);
});

test("build action eligibility requires current quality and verified files", () => {
  const analysis = {
    generated_at: "now", model: "m", mode: "agentic", critique_passed: true, critique_version: 7,
    root_cause: "cause", severity: "High", suggested_fix: "Use `MissingHelper`.",
  };
  assert.equal(buildActionEligibilityHint(analysis, 7)?.state, "more_evidence_required");
  assert.equal(buildActionEligibilityHint({ ...analysis, file_links: { "main.go": "https://example.test/main.go" } }, 7), null);
  assert.equal(buildActionEligibilityHint({ ...analysis, file_links: { "main.go": "https://example.test/main.go" } }, 8)?.state, "more_evidence_required");
});

test("action eligibility titles explain each state", () => {
  assert.equal(actionEligibilityTitle(eligibilityForState("already_present"), false), "Remediation already exists");
  assert.equal(actionEligibilityTitle(eligibilityForState("more_evidence_required"), false), "More source evidence required");
  assert.equal(actionEligibilityTitle(eligibilityForState("investigation_required"), true), "Investigate source");
  assert.equal(actionEligibilityTitle(eligibilityForState("investigation_required"), false), "Source investigation is not configured");
});
