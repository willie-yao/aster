import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  notInvestigatedReason,
  patternRemediationPresentation,
} from "../src/lib/patternRemediation.js";
import type { PatternRemediationInvestigationState } from "../src/types/dashboard.js";

const states: PatternRemediationInvestigationState[] = [
  "not_investigated",
  "investigating",
  "actionable",
  "already_fixed",
  "external_dependency",
  "environment_or_infrastructure",
  "mitigation_only",
  "insufficient_evidence",
  "failed",
];

test("causal remediation defaults to an explicit non-actionable state", () => {
  assert.deepEqual(patternRemediationPresentation(), {
    state: "not_investigated",
    label: "Not investigated",
    message: notInvestigatedReason,
    futureAction: "Investigate possible fix",
    detail: undefined,
  });
});

test("every remediation investigation state has user-visible copy", () => {
  for (const state of states) {
    const presentation = patternRemediationPresentation({
      causal_group_id: "group",
      causal_group_hash: "hash",
      state,
    });
    assert.equal(presentation.state, state);
    assert.ok(presentation.label);
    assert.ok(presentation.message);
  }
});

test("technical reasons remain separate from the concise state message", () => {
  const presentation = patternRemediationPresentation({
    causal_group_id: "group",
    causal_group_hash: "hash",
    state: "failed",
    reason: "The pinned source revision was unavailable.",
  });
  assert.match(presentation.message, /Published causal analysis is unchanged/);
  assert.equal(presentation.detail, "The pinned source revision was unavailable.");
});


test("PR 1 renders status without a clickable investigation action", () => {
  const component = readFileSync(resolve(process.cwd(), "src/components/PatternRemediation.tsx"), "utf8");
  assert.match(component, />\s*Remediation\s*</);
  assert.match(component, /aria-live="polite"/);
  assert.match(component, /Investigation details/);
  assert.doesNotMatch(component, /<Button|onClick=/);
});
