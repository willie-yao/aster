import assert from "node:assert/strict";
import test from "node:test";
import {
  actionEligibilityTitle,
  buildActionEligibilityHint,
  eligibilityForCode,
  eligibilityForState,
  normalizeActionEligibility,
  patternActionEligibilityHint,
  patternResolvable,
  patternDraftable,
  patternLifecycleActive,
  selectActionEligibility,
} from "../src/lib/actionEligibility.js";
import type { PatternAnalysis } from "../src/types/dashboard.js";

const actionableTarget = { intent: "add_symbol" as const, path: "main.go", symbol: "MissingHelper" };

const basePattern: PatternAnalysis = {
  id: "pattern-1",
  subject: "periodic-x",
  generated_at: "2026-08-18T00:00:00Z",
  builds_analyzed: 3,
  systemic: true,
  confidence: "high",
  shared_builds: ["100", "250"],
  summary: "etcd leader election times out",
};

test("pattern action eligibility handles deterministic blocked states", () => {
  assert.equal(patternActionEligibilityHint(undefined)?.code, "contract_generation_failed");
  assert.equal(patternActionEligibilityHint([{ intent: "investigate" }])?.state, "investigation_required");
  assert.equal(patternActionEligibilityHint([{ intent: "add_symbol", path: "main.go" }])?.state, "more_evidence_required");
  assert.equal(patternActionEligibilityHint([{ intent: "modify_symbol", path: "main.go", symbol: "reconcile" }])?.state, "more_evidence_required");
  assert.equal(patternActionEligibilityHint([{
    intent: "modify_symbol", path: "main.go", symbol: "reconcile", required_call: "ApplyFix",
  }]), null);
  assert.equal(patternActionEligibilityHint([actionableTarget]), null);
  assert.equal(patternActionEligibilityHint([{
    intent: "set_job_environment",
    repository: "kubernetes/test-infra",
    revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    path: "config/jobs/kubernetes-sigs/cluster-api-provider-azure/periodics.yaml",
    job: "periodic-capz",
    container: "test",
    name: "AKS_MGMT_KUBERNETES_VERSION",
    value: "v1.34.1",
  }]), null);
  const observing = { state: "observing" as const, reason: "remediation present" };
  assert.equal(patternLifecycleActive(observing), false);
  assert.equal(patternActionEligibilityHint([actionableTarget], observing)?.state, "already_present");
  assert.equal(patternActionEligibilityHint([actionableTarget], observing)?.reason, "remediation present");
  const recovered = { state: "recovered" as const, reason: "three observed passes", recovery_streak: 3 };
  assert.equal(patternLifecycleActive(recovered), false);
  assert.equal(patternActionEligibilityHint([actionableTarget], recovered)?.state, "recovered");
  assert.equal(patternActionEligibilityHint([actionableTarget], recovered)?.reason, "three observed passes");
  assert.equal(patternLifecycleActive(undefined), true);
});

test("build action eligibility requires current quality and verified files", () => {
  const analysis = {
    generated_at: "now", mode: "agentic", disposition: "citations_verified" as const, critique_passed: true, critique_version: 7,
    root_cause: "cause", severity: "High", suggested_fix: "Use `MissingHelper`.",
  };
  assert.equal(buildActionEligibilityHint(analysis, 7)?.state, "more_evidence_required");
  assert.equal(buildActionEligibilityHint({ ...analysis, file_links: { "main.go": "https://example.test/main.go" } }, 7), null);
  assert.equal(buildActionEligibilityHint({ ...analysis, file_links: { "main.go": "https://example.test/main.go" } }, 8)?.state, "more_evidence_required");
});

test("action eligibility titles explain each state", () => {
  assert.equal(actionEligibilityTitle(eligibilityForState("already_present")), "Remediation already exists");
  assert.equal(actionEligibilityTitle(eligibilityForState("recovered")), "Watching recovery");
  assert.equal(actionEligibilityTitle(eligibilityForState("more_evidence_required")), "Current evidence unavailable");
  assert.equal(actionEligibilityTitle(eligibilityForState("investigation_required")), "Investigation required");
});


test("structured action reasons distinguish safe blocked states", () => {
  assert.equal(actionEligibilityTitle(eligibilityForCode("non_systemic")), "Not a recurring systemic pattern");
  assert.equal(actionEligibilityTitle(eligibilityForCode("unsafe_remediation")), "Unsafe remediation blocked");
  assert.equal(actionEligibilityTitle(eligibilityForCode("observing")), "Observing verified remediation");
  assert.equal(actionEligibilityTitle(eligibilityForCode("verified_fixed")), "Verified fixed");
  // A retained correlation is not a blocker; unreadable evidence is.
  assert.equal(patternActionEligibilityHint([actionableTarget], undefined, true, { state: "retained", evidence_available: true }), null);
  assert.equal(patternActionEligibilityHint([actionableTarget], undefined, true, { state: "retained", evidence_available: false })?.code, "evidence_unavailable");
  assert.equal(patternActionEligibilityHint([actionableTarget], undefined, false)?.code, "non_systemic");
});

test("pattern drafting stays independent of dismissal", () => {
  const legacy: PatternAnalysis = { ...basePattern };

  assert.equal(patternDraftable(legacy), true);
  // Causal-group results publish per-cause remediation, not a pattern contract.
  assert.equal(patternDraftable({ ...legacy, recurrence_classification: "shared_cause" }), false);
  // A missing watermark blocks dismissal but must not block drafting.
  assert.equal(patternResolvable({ ...legacy, shared_builds: [] }), false);
  assert.equal(patternDraftable({ ...legacy, shared_builds: [] }), true);

  assert.equal(patternDraftable({ ...legacy, systemic: false }), false);
  assert.equal(patternDraftable({ ...legacy, id: undefined }), false);
  assert.equal(patternDraftable({ ...legacy, lifecycle: { state: "observing", reason: "r" } }), false);
  assert.equal(patternDraftable(legacy, { state: "retained", evidence_available: true }), true);
  assert.equal(patternDraftable(legacy, { state: "retained", evidence_available: false }), false);
});

test("legacy eligibility payloads derive a compatible reason code", () => {
  const legacy = normalizeActionEligibility({ state: "investigation_required", reason: "legacy reason" });
  assert.equal(legacy.code, "investigation_required");
  assert.equal(legacy.reason, "legacy reason");
});

test("pattern dismissal never depends on the causal-group classification", () => {
  // Every pattern the engine publishes carries a recurrence_classification, so
  // keying visibility off it hid the control everywhere. This pins that it does
  // not.
  const causalGroup: PatternAnalysis = {
    ...basePattern,
    recurrence_classification: "shared_cause",
    causal_groups: [{ builds: ["100", "250"], root_cause: "etcd timeout", confidence: "high" }],
  };

  assert.equal(patternResolvable(causalGroup), true);
  assert.equal(patternResolvable({ ...causalGroup, recurrence_classification: "mixed_causes" }), true);
  assert.equal(patternResolvable(basePattern), true);
});

test("pattern dismissal matches the gates the server enforces", () => {
  const causalGroup: PatternAnalysis = { ...basePattern, recurrence_classification: "shared_cause" };

  assert.equal(patternResolvable({ ...causalGroup, id: "" }), false);
  assert.equal(patternResolvable({ ...causalGroup, id: undefined }), false);
  assert.equal(patternResolvable({ ...causalGroup, systemic: false }), false);
  for (const state of ["observing", "recovered", "verified_fixed"] as const) {
    assert.equal(patternResolvable({ ...causalGroup, lifecycle: { state, reason: "r" } }), false);
  }
  assert.equal(patternResolvable({ ...causalGroup, lifecycle: { state: "active", reason: "r" } }), true);

  // The server derives the recurrence watermark from shared_builds and refuses a
  // pattern with no usable build history.
  assert.equal(patternResolvable({ ...causalGroup, shared_builds: undefined }), false);
  assert.equal(patternResolvable({ ...causalGroup, shared_builds: [] }), false);
  assert.equal(patternResolvable({ ...causalGroup, shared_builds: ["not-a-build"] }), false);

  // findPattern rejects a pattern whose evidence has left the job window, or
  // whose correlation failed outright. A retained correlation is not a blocker.
  assert.equal(patternResolvable(causalGroup, { state: "current", evidence_available: true }), true);
  assert.equal(patternResolvable(causalGroup, { state: "current", evidence_available: false }), false);
  assert.equal(patternResolvable(causalGroup, { state: "retained", evidence_available: true }), true);
  assert.equal(patternResolvable(causalGroup, { state: "retained", evidence_available: false }), false);
  assert.equal(patternResolvable(causalGroup, { state: "failed", evidence_available: true }), false);
});

test("action eligibility explanations use a polite status surface", async () => {
  const source = await import("node:fs/promises").then((fs) => fs.readFile("src/components/FailureActions.tsx", "utf8"));
  assert.match(source, /<Alert role="status" severity=\{eligibility\.state/);
  assert.match(source, /actionEligibilityTitle\(eligibility/);
  assert.match(source, />\s*Draft issue\s*</);
  assert.match(source, />\s*Draft fix PR\s*</);
  assert.match(source, />\s*Resolve pattern\s*</);
  assert.match(source, />\s*Reopen pattern\s*</);
  assert.match(source, /Review issue draft/);
  assert.doesNotMatch(source, />\s*Mark resolved\s*</);
});


test("component eligibility selection preserves structured hint codes", () => {
  for (const code of ["evidence_unavailable", "non_systemic", "unsafe_remediation"] as const) {
    const hint = eligibilityForCode(code);
    assert.equal(selectActionEligibility(hint, null, "pattern")?.code, code);
  }
  const fetched = eligibilityForCode("source_verification_inconclusive");
  assert.equal(selectActionEligibility(undefined, { failureID: "pattern", value: fetched }, "pattern")?.code, "source_verification_inconclusive");
});


test("actionable null hints fall through to authoritative fetched eligibility", () => {
  const actionable = eligibilityForCode("actionable");
  const fetched = { failureID: "pattern", value: actionable };
  assert.equal(selectActionEligibility(undefined, fetched, "pattern")?.code, "actionable");
  assert.equal(selectActionEligibility(null, fetched, "pattern")?.code, "actionable");
  assert.equal(selectActionEligibility(eligibilityForCode("evidence_unavailable"), fetched, "pattern")?.code, "evidence_unavailable");
  assert.equal(selectActionEligibility(null, fetched, "other"), null);
});
