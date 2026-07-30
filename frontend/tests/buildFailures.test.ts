import assert from "node:assert/strict";
import test from "node:test";
import { buildActionsReady, buildAnalysisState, buildFailure, buildFailureActionID, junitTestCases } from "../src/lib/buildFailures.js";
import type { FetchProgressStatus, FetchStatusResponse } from "../src/types/fetchStatus.js";
import type { TestCase } from "../src/types/dashboard.js";

const failure: TestCase = { name: "Prow job execution", source: "build", status: "failed", duration_seconds: 1 };
const status: FetchProgressStatus = {
  schema_version: 6, run_id: "run", pass_id: "pass", pass_type: "initial-watch", phase: "analysis",
  run_started_at: "2026-07-30T00:00:00Z", pass_started_at: "2026-07-30T00:00:00Z",
  phase_started_at: "2026-07-30T00:00:00Z", last_progress_at: "2026-07-30T00:00:00Z", outcome: "running",
  jobs: { total: 1, completed: 1 }, builds: { cached: 0, fetched: 1 },
  analyses: {
    logical_total: 1, accepted_cache_hits: 0, compatible_results_reused: 0, new_work: 1, stale_work: 0, queued: 1, running: 0,
    completed: 0, failed: 0, cancelled: 0, task_attempts: 0, retries: 0, existing_tasks_adopted: 0,
    results_retrieved: 0, result_retrieval_retries: 0,
    build_subjects: { logical_total: 1, queued: 1, running: 0, completed: 0, failed: 0, cancelled: 0, accepted_cache_hits: 0, existing_tasks_adopted: 0 },
  },
  pattern_phase: "pending", publication_phase: "pending", side_effect_phase: "pending",
};
const response = (state: FetchStatusResponse["state"], value = status): FetchStatusResponse => ({ available: true, state, status: value });

test("build analysis state covers pending success unavailable and stale", () => {
  assert.equal(buildAnalysisState(failure, response("active")), "pending");
  assert.equal(buildAnalysisState(failure, response("active", { ...status, analyses: { ...status.analyses, logical_total: 2, queued: 1, running: 1, build_subjects: { ...status.analyses.build_subjects!, logical_total: 2, queued: 1, running: 1 } } })), "pending");
  assert.equal(buildAnalysisState(failure, response("active", { ...status, analyses: { ...status.analyses, queued: 0, running: 1, build_subjects: { ...status.analyses.build_subjects!, queued: 0, running: 1 } } })), "pending");
  assert.equal(buildAnalysisState({ ...failure, ai_analysis: { generated_at: "now", model: "m", root_cause: "cause", severity: "High", suggested_fix: "fix" } }, null), "succeeded");
  assert.equal(buildAnalysisState(failure, null), "unavailable");
  assert.equal(buildAnalysisState(failure, response("stale")), "stale");
});


test("build subjects stay out of JUnit-only collections", () => {
  const junit: TestCase = { name: "real test", status: "failed", duration_seconds: 1 };
  assert.deepEqual(junitTestCases([failure, junit]), [junit]);
  assert.equal(buildFailure([junit, failure]), failure);
});


test("build action IDs are stable and source-scoped", () => {
  assert.equal(buildFailureActionID("periodic-aks", "123"), "build::cGVyaW9kaWMtYWtz::MTIz");
  assert.notEqual(buildFailureActionID("periodic-aks", "123"), buildFailureActionID("periodic-aks", "124"));
});


test("build actions require the server's current critique contract", () => {
  const analysis = { generated_at: "now", model: "m", mode: "agentic", critique_passed: true, critique_version: 7, root_cause: "cause", severity: "High", suggested_fix: "fix" };
  assert.equal(buildActionsReady(analysis, 7), true);
  assert.equal(buildActionsReady(analysis, 8), false);
  assert.equal(buildActionsReady(analysis, undefined), false);
});
