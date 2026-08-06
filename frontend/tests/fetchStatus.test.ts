import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";
import {
  analysisProgressAccessibleDetail,
  analysisProgressBreakdown,
  fetchStatusCompactPresentation,
  fetchStatusHasCompletedPipeline,
  fetchStatusMacroStages,
  fetchStatusPresentation,
  fetchStatusStripKey,
  formatFetchRelativeTime,
  nextFetchStatusDelay,
  nextFetchTime,
  pollFetchStatus,
  shouldShowFetchStatusStrip,
} from "../src/lib/fetchStatus.js";
import type { FetchProgressStatus, FetchStatusResponse } from "../src/types/fetchStatus.js";

const fetchStatusSource = readFileSync(resolve(process.cwd(), "src/components/FetchStatus.tsx"), "utf8");

const activeStatus: FetchProgressStatus = {
  schema_version: 6,
  run_id: "safe-run",
  pass_id: "safe-pass",
  pass_type: "lightweight-watch",
  phase: "analysis",
  run_started_at: "2026-07-28T10:00:00Z",
  pass_started_at: "2026-07-28T11:00:00Z",
  phase_started_at: "2026-07-28T11:01:00Z",
  last_progress_at: "2026-07-28T11:02:00Z",
  last_checked_at: "2026-07-28T11:00:30Z",
  last_successful_publication_at: "2026-07-28T10:55:00Z",
  outcome: "running",
  jobs: { total: 28, completed: 28 },
  builds: { cached: 241, fetched: 29 },
  analyses: {
    logical_total: 126,
    accepted_cache_hits: 68,
    compatible_results_reused: 2,
    exact_results_reused: 12,
    same_failure_results_reused: 5,
    same_failure_groups: 3,
    same_failure_candidates: 9,
    potential_tasks_saved: 6,
    largest_same_failure_group: 4,
    new_work: 44,
    stale_work: 0,
    cache_rejections: {
      missing: 44,
      expired: 0,
      tool_floor: 0,
      evidence_floor: 0,
      critique: 0,
      malformed: 0,
    },
    queued: 40,
    running: 2,
    completed: 84,
    failed: 0,
    cancelled: 0,
    task_attempts: 4,
    retries: 0,
    existing_tasks_adopted: 0,
    new_tasks_created: 4,
    results_retrieved: 2,
    fresh_analyses_completed: 2,
    result_retrieval_retries: 0,
  },
  pattern_phase: "pending",
  publication_phase: "pending",
  side_effect_phase: "pending",
};

function response(state: FetchStatusResponse["state"], status: FetchProgressStatus = activeStatus): FetchStatusResponse {
  return { available: true, state, stale: state === "stale", status };
}

test("analysis progress distinguishes reuse, adoption, and new Tasks", () => {
  const progress = analysisProgressBreakdown(activeStatus);
  assert.deepEqual(progress, {
    total: 126,
    ready: 84,
    reusedFromCache: 68,
    compatibleResults: 2,
    reused: 70,
    exactResultsReused: 12,
    sameFailureResultsReused: 5,
    sameFailureGroups: 3,
    sameFailureCandidates: 9,
    potentialTasksSaved: 6,
    largestSameFailureGroup: 4,
    lateTasksAdopted: 0,
    newTasksCreated: 4,
    freshAnalysesCompleted: 2,
    analyzing: 2,
    waiting: 40,
    failed: 0,
    cancelled: 0,
    terminal: 84,
  });
  assert.equal(
    analysisProgressAccessibleDetail(progress),
    "84 of 126 results ready: 70 reused, 12 exact results reused, 5 same-failure results reused, 0 existing Tasks adopted, 2 newly analyzed, 2 running, 40 waiting, 6 potential same-failure Task savings",
  );
});

test("late Task adoption stays separate from exact reuse and fresh work", () => {
  const progress = analysisProgressBreakdown({
    ...activeStatus,
    analyses: {
      ...activeStatus.analyses,
      exact_results_reused: 5,
      existing_tasks_adopted: 1,
      new_tasks_created: 3,
      fresh_analyses_completed: 2,
    },
  });
  assert.equal(progress.exactResultsReused, 5);
  assert.equal(progress.lateTasksAdopted, 1);
  assert.equal(progress.newTasksCreated, 3);
  assert.equal(progress.freshAnalysesCompleted, 2);
});

test("analysis progress keeps exact counters non-negative", () => {
  const progress = analysisProgressBreakdown({
    ...activeStatus,
    analyses: {
      ...activeStatus.analyses,
      logical_total: -1,
      accepted_cache_hits: -2,
      compatible_results_reused: -3,
      task_attempts: -8,
      retries: -2,
      existing_tasks_adopted: -3,
      new_tasks_created: -4,
      results_retrieved: -1,
      fresh_analyses_completed: -2,
      exact_results_reused: -5,
      completed: -4,
      running: -5,
      queued: -6,
      failed: 1,
      cancelled: 1,
    },
  });
  assert.equal(progress.total, 0);
  assert.equal(progress.reused, 0);
  assert.equal(progress.exactResultsReused, 0);
  assert.equal(progress.lateTasksAdopted, 0);
  assert.equal(progress.newTasksCreated, 0);
  assert.equal(progress.freshAnalysesCompleted, 0);
  assert.equal(progress.ready, 0);
  assert.equal(progress.analyzing, 0);
  assert.equal(progress.waiting, 0);
  assert.equal(progress.terminal, 2);
});

test("active analysis status is phase-first with meaningful determinate progress", () => {
  const status: FetchProgressStatus = {
    ...activeStatus,
    analyses: {
      ...activeStatus.analyses,
      logical_total: 79,
      completed: 43,
      running: 2,
      queued: 34,
    },
  };
  const presentation = fetchStatusPresentation(response("active", status));
  const compact = fetchStatusCompactPresentation(response("active", status));

  assert.equal(presentation?.title, "Analyzing failures");
  assert.equal(presentation?.detail, "43 of 79 analyses ready");
  assert.equal(presentation?.announcement, "Analyzing failures");
  assert.equal(presentation?.determinateTotal, 79);
  assert.equal(presentation?.determinateCompleted, 43);
  assert.equal(compact?.label, "Analyzing failures · 43/79");
  assert.match(compact?.ariaLabel ?? "", /Analyzing failures/);
  assert.match(compact?.ariaLabel ?? "", /43 of 79 analyses ready/);
});

test("completed analyses do not override pattern finalization", () => {
  const status: FetchProgressStatus = {
    ...activeStatus,
    phase: "patterns",
    analyses: {
      ...activeStatus.analyses,
      logical_total: 79,
      completed: 79,
      running: 0,
      queued: 0,
    },
    pattern_phase: "running",
  };
  const statusResponse = response("active", status);
  const presentation = fetchStatusPresentation(statusResponse);
  const compact = fetchStatusCompactPresentation(statusResponse);
  const stages = fetchStatusMacroStages(statusResponse);

  assert.equal(presentation?.title, "Finalizing recurring patterns");
  assert.equal(presentation?.detail, "79 analyses ready");
  assert.equal(presentation?.determinateTotal, null);
  assert.equal(compact?.label, "Finalizing patterns");
  assert.doesNotMatch(compact?.label ?? "", /79\/79|ready/i);
  assert.deepEqual(stages.map((stage) => [stage.label, stage.stateLabel]), [
    ["Fetch data", "Complete"],
    ["Analyze", "Complete"],
    ["Patterns", "Active"],
    ["Publish", "Pending"],
  ]);
});

test("early refresh phases use user-facing labels and scoped progress", () => {
  const cases: Array<[FetchProgressStatus["phase"], string]> = [
    ["setup", "Preparing refresh"],
    ["discovery", "Discovering jobs"],
    ["aggregation", "Building dashboard"],
    ["analysis-planning", "Planning analyses"],
  ];
  for (const [phase, label] of cases) {
    assert.equal(fetchStatusCompactPresentation(response("active", { ...activeStatus, phase }))?.label, label);
  }
  const artifactResponse = response("active", {
    ...activeStatus,
    phase: "artifacts",
    jobs: { total: 28, completed: 18 },
  });
  const artifacts = fetchStatusCompactPresentation(artifactResponse);
  const artifactPresentation = fetchStatusPresentation(artifactResponse);
  assert.equal(artifacts?.label, "Fetching runs · 18/28");
  assert.equal(artifactPresentation?.determinateTotal, 28);
  assert.equal(artifactPresentation?.determinateCompleted, 18);
});

test("publication and side effects retain phase-first labels", () => {
  const publication = fetchStatusCompactPresentation(response("active", {
    ...activeStatus,
    phase: "publication",
    publication_phase: "running",
  }));
  const sideEffects = fetchStatusCompactPresentation(response("active", {
    ...activeStatus,
    phase: "side-effects",
    publication_phase: "completed",
    side_effect_phase: "running",
  }));

  assert.equal(publication?.label, "Publishing dashboard");
  assert.equal(sideEffects?.label, "Finishing refresh");
});

test("idle status is up to date and exposes future checks", () => {
  const idleStatus: FetchProgressStatus = {
    ...activeStatus,
    phase: "idle",
    outcome: "succeeded",
    next_watch_at: "2026-07-28T11:10:00Z",
    next_reconcile_at: "2026-07-28T12:00:00Z",
  };
  const presentation = fetchStatusPresentation(response("idle", idleStatus));
  const compact = fetchStatusCompactPresentation(response("idle", idleStatus));

  assert.equal(presentation?.title, "Up to date");
  assert.equal(presentation?.severity, "success");
  assert.equal(compact?.label, "Up to date");
  assert.ok(nextFetchTime(idleStatus)?.includes("Next reconcile"));
  assert.equal(formatFetchRelativeTime("2026-07-28T11:10:00Z", Date.parse("2026-07-28T11:00:00Z")), "in 10m");
});

test("stale idle schedules keep the completed pipeline distinct from the overdue check", () => {
  const staleIdle = response("stale", {
    ...activeStatus,
    phase: "idle",
    outcome: "succeeded",
    pattern_phase: "completed",
    publication_phase: "completed",
    side_effect_phase: "completed",
  });
  const presentation = fetchStatusPresentation(staleIdle);
  const stages = fetchStatusMacroStages(staleIdle);

  assert.equal(presentation?.title, "Status stale");
  assert.equal(presentation?.detail, "The next scheduled refresh check is overdue");
  assert.equal(fetchStatusHasCompletedPipeline(staleIdle), true);
  assert.deepEqual(stages.map((stage) => stage.stateLabel), ["Complete", "Complete", "Complete", "Complete"]);
  assert.equal(shouldShowFetchStatusStrip(staleIdle), true);
  assert.match(fetchStatusSource, /const completedPipeline = fetchStatusHasCompletedPipeline\(response\)/);
  assert.match(fetchStatusSource, /\{completedPipeline \? \([\s\S]*Next check/);
  assert.match(fetchStatusSource, /\{!completedPipeline && \(/);
});

test("degraded states retain severity and the persistent alert strip", () => {
  const failed = response("failed", {
    ...activeStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "publication",
    publication_phase: "failed",
  });
  const stale = response("stale");
  const interrupted = response("interrupted", {
    ...activeStatus,
    phase: "interrupted",
    outcome: "interrupted",
  });

  assert.equal(fetchStatusPresentation(failed)?.title, "Refresh failed");
  assert.equal(fetchStatusPresentation(failed)?.severity, "error");
  assert.equal(fetchStatusPresentation(stale)?.title, "Status stale");
  assert.equal(fetchStatusPresentation(stale)?.severity, "warning");
  assert.equal(fetchStatusPresentation(interrupted)?.title, "Refresh interrupted");
  assert.equal(fetchStatusPresentation(interrupted)?.severity, "warning");
  assert.equal(shouldShowFetchStatusStrip(failed), true);
  assert.equal(shouldShowFetchStatusStrip(stale), true);
  assert.equal(shouldShowFetchStatusStrip(interrupted), true);
  assert.equal(shouldShowFetchStatusStrip(response("cancelled")), true);
  assert.equal(shouldShowFetchStatusStrip(response("active")), false);
  assert.equal(shouldShowFetchStatusStrip(response("idle")), false);
  assert.equal(fetchStatusStripKey(response("failed")), "safe-pass:failed");
  assert.equal(fetchStatusPresentation({ available: false, state: "missing" }), null);
});

test("default popover stays user-facing and technical details retain diagnostics", () => {
  const technicalIndex = fetchStatusSource.indexOf("Technical details");
  assert.ok(technicalIndex > 0);
  const summarySource = fetchStatusSource.slice(0, technicalIndex);
  const technicalSource = fetchStatusSource.slice(technicalIndex);

  for (const label of [
    "Compatible results",
    "Exact results reused",
    "Same-failure results reused",
    "Existing Tasks adopted",
    "New analyzer Tasks",
    "Task attempts",
    "Same-failure candidates",
    "Cache rejections",
    "Phase durations",
    "Run ID",
    "Pass ID",
    "Recent passes",
  ]) {
    assert.doesNotMatch(summarySource, new RegExp(`label="${label}"`));
    assert.match(technicalSource, new RegExp(label));
  }
  assert.match(summarySource, /Last published/);
  assert.match(summarySource, /Current pass began/);
  assert.match(summarySource, /Last activity/);
  assert.match(summarySource, /Next check/);
  assert.match(summarySource, /Next full reconciliation/);
  assert.match(summarySource, /last published dashboard remains available/);
  assert.match(fetchStatusSource, /<Collapse in=\{technicalOpen\} timeout=\{reduceMotion \? 0 : "auto"\}>/);
});

test("phase changes are announced politely without counter churn", () => {
  const liveStart = fetchStatusSource.indexOf('role="status" aria-live="polite" aria-atomic="true"');
  const liveEnd = fetchStatusSource.indexOf("</Box>", liveStart);
  const liveSource = fetchStatusSource.slice(liveStart, liveEnd);
  assert.ok(liveStart > 0 && liveEnd > liveStart);
  assert.match(liveSource, /\{presentation\.announcement\}/);
  assert.doesNotMatch(liveSource, /determinateCompleted|analysis\.|jobs\./);
  assert.match(fetchStatusSource, /component="ol"[\s\S]*aria-label="Refresh stages"/);
  assert.match(fetchStatusSource, /aria-label=\{`\$\{stage\.label\}: \$\{stage\.stateLabel\}`\}/);
  assert.match(fetchStatusSource, /\{stage\.stateLabel\}/);
  assert.match(fetchStatusSource, /minWidth: \{ xs: 44, md: "auto" \}/);
  assert.match(fetchStatusSource, /component="time"[\s\S]*dateTime=\{value\}[\s\S]*tabIndex=\{0\}/);
  assert.match(fetchStatusSource, /"&:focus-visible"/);
  assert.match(fetchStatusSource, /prefers-reduced-motion: reduce/);
});

test("polling is sequential and backs off after failures", async () => {
  const delays: number[] = [];
  const states: string[] = [];
  let calls = 0;
  const controller = new AbortController();
  await pollFetchStatus({
    url: "/api/fetch-status",
    signal: controller.signal,
    baseDelay: 100,
    maxDelay: 800,
    fetcher: async () => {
      calls++;
      if (calls <= 2) return new Response("failed", { status: 500 });
      if (calls === 3) return new Response(JSON.stringify(response("active")), { status: 200, headers: { "Content-Type": "application/json" } });
      return new Response("missing", { status: 404 });
    },
    wait: async (delay) => { delays.push(delay); },
    onStatus: (value) => states.push(value.state),
  });
  assert.equal(calls, 4);
  assert.deepEqual(delays, [100, 200, 100]);
  assert.deepEqual(states, ["active"]);
  assert.equal(nextFetchStatusDelay("idle", 0, 100, 800), 200);
  assert.equal(nextFetchStatusDelay("active", 0, 100, 800), 100);
});

test("polling aborts an in-flight request without another poll", async () => {
  const controller = new AbortController();
  let calls = 0;
  const polling = pollFetchStatus({
    url: "/api/fetch-status",
    signal: controller.signal,
    fetcher: async (_input, init) => {
      calls++;
      return await new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("cancelled", "AbortError")), { once: true });
      });
    },
    wait: async () => { throw new Error("wait should not run"); },
    onStatus: () => { throw new Error("status should not update"); },
  });
  await Promise.resolve();
  controller.abort();
  await polling;
  assert.equal(calls, 1);
});

test("endpoint absence stops polling without surfacing an error", async () => {
  const controller = new AbortController();
  let updates = 0;
  let waits = 0;
  await pollFetchStatus({
    url: "/api/fetch-status",
    signal: controller.signal,
    fetcher: async () => new Response("missing", { status: 404 }),
    wait: async () => { waits++; },
    onStatus: () => { updates++; },
  });
  assert.equal(updates, 0);
  assert.equal(waits, 0);
});
