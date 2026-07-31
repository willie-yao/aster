import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";
import {
  FETCH_STATUS_IDLE_COMPACT_KEY,
  analysisProgressAccessibleDetail,
  analysisProgressBreakdown,
  fetchStatusCompactPresentation,
  fetchStatusPresentation,
  fetchStatusStripKey,
  nextFetchStatusDelay,
  nextFetchTime,
  pollFetchStatus,
  readFetchStatusIdleCompact,
  resolveFetchStatusPreferenceStorage,
  writeFetchStatusIdleCompact,
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
    task_attempts: 16,
    retries: 0,
    existing_tasks_adopted: 12,
    results_retrieved: 14,
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
    adopted: 12,
    newAnalyzerTasks: 4,
    newlyAnalyzed: 2,
    analyzing: 2,
    waiting: 40,
    failed: 0,
    cancelled: 0,
    terminal: 84,
  });
  assert.equal(
    analysisProgressAccessibleDetail(progress),
    "84 of 126 results ready: 70 reused, 12 existing results adopted, 2 newly analyzed, 2 running, 40 waiting",
  );
});

test("analysis progress stays non-negative and removes retries from the new Task count", () => {
  const progress = analysisProgressBreakdown({
    ...activeStatus,
    analyses: {
      ...activeStatus.analyses,
      logical_total: -1,
      accepted_cache_hits: -2,
      compatible_results_reused: -3,
      task_attempts: 8,
      retries: 2,
      existing_tasks_adopted: 3,
      results_retrieved: 1,
      completed: -4,
      running: -5,
      queued: -6,
      failed: 1,
      cancelled: 1,
    },
  });
  assert.equal(progress.total, 0);
  assert.equal(progress.reused, 0);
  assert.equal(progress.newAnalyzerTasks, 3);
  assert.equal(progress.newlyAnalyzed, 0);
  assert.equal(progress.ready, 0);
  assert.equal(progress.analyzing, 0);
  assert.equal(progress.waiting, 0);
  assert.equal(progress.terminal, 2);
});

test("fetch status presentation covers active idle failed and stale states", () => {
  const active = fetchStatusPresentation(response("active"));
  assert.equal(active?.title, "84 of 126 results ready");
  assert.equal(active?.detail, "70 reused · 12 adopted · 2 new · 2 analyzing · 40 waiting");
  assert.equal(active?.announcement, "Fetch in progress: Analysis");
  assert.equal(active?.determinateTotal, 126);
  assert.equal(active?.determinateCompleted, 84);
  assert.equal(
    active?.ariaLabel,
    "Fetch in progress: Analysis. 84 of 126 results ready: 70 reused, 12 existing results adopted, 2 newly analyzed, 2 running, 40 waiting.",
  );

  const terminalFailures = fetchStatusPresentation(response("active", {
    ...activeStatus,
    analyses: { ...activeStatus.analyses, completed: 82, failed: 1, cancelled: 1 },
  }));
  assert.equal(terminalFailures?.title, "82 of 126 results ready");
  assert.equal(terminalFailures?.determinateCompleted, 84);
  assert.match(terminalFailures?.detail ?? "", /1 failed · 1 cancelled/);

  const idle = fetchStatusPresentation(response("idle", {
    ...activeStatus,
    phase: "idle",
    outcome: "succeeded",
    next_watch_at: "2026-07-28T11:10:00Z",
    next_reconcile_at: "2026-07-28T12:00:00Z",
  }));
  assert.equal(idle?.title, "Fetch idle");
  assert.ok(nextFetchTime(response("idle", {
    ...activeStatus,
    phase: "idle",
    outcome: "succeeded",
    next_watch_at: "2026-07-28T11:10:00Z",
    next_reconcile_at: "2026-07-28T12:00:00Z",
  }).status!)?.includes("Next reconcile"));

  const failed = fetchStatusPresentation(response("failed", {
    ...activeStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "publication",
  }));
  assert.equal(failed?.title, "Fetch failed: Publication");
  assert.equal(failed?.severity, "error");

  const stale = fetchStatusPresentation(response("stale"));
  assert.equal(stale?.title, "Fetch status stale: Analysis");
  assert.equal(stale?.severity, "warning");

  const interrupted = fetchStatusPresentation(response("interrupted", { ...activeStatus, phase: "interrupted", outcome: "interrupted" }));
  assert.equal(interrupted?.title, "Previous fetch interrupted");
  assert.equal(interrupted?.severity, "warning");

  assert.equal(fetchStatusPresentation({ available: false, state: "missing" }), null);
});

test("compact fetch status distinguishes quiet and attention states", () => {
  const active = fetchStatusCompactPresentation(response("active"));
  assert.equal(active?.label, "84/126 ready");
  assert.match(active?.ariaLabel ?? "", /70 reused/);
  assert.equal(active?.quiet, false);
  assert.equal(active?.severity, "info");

  const idle = fetchStatusCompactPresentation(response("idle", {
    ...activeStatus,
    phase: "idle",
    outcome: "succeeded",
  }));
  assert.equal(idle?.label, "Idle");
  assert.equal(idle?.quiet, true);
  assert.equal(idle?.severity, "success");

  const failed = fetchStatusCompactPresentation(response("failed", {
    ...activeStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "patterns",
  }));
  assert.equal(failed?.label, "Fetch failed");
  assert.equal(failed?.quiet, false);
  assert.equal(failed?.severity, "error");
  assert.equal(fetchStatusStripKey(response("active")), "safe-pass:active");
  assert.equal(fetchStatusStripKey(response("failed")), "safe-pass:failed");
});


test("fetch status popover presents the user-facing breakdown before technical counters", () => {
  for (const label of [
    "Results ready",
    "Reused from cache",
    "Compatible results",
    "Existing results adopted",
    "New analyzer Tasks",
    "Currently analyzing",
    "Waiting to check",
    "Failures",
    "Technical details",
  ]) {
    assert.match(fetchStatusSource, new RegExp(label));
  }
  assert.match(fetchStatusSource, /including adopted existing Tasks/);
  assert.match(fetchStatusSource, /<Collapse in=\{technicalOpen\}/);
  assert.match(fetchStatusSource, /<CircularProgress\s+aria-hidden="true"\s+role="presentation"/);
});

test("polling counters are not inside the live status announcement", () => {
  assert.match(fetchStatusSource, /role="region"[\s\S]*aria-label=\{presentation\.ariaLabel\}/);
  assert.match(fetchStatusSource, /role="status"[\s\S]*aria-live="polite"[\s\S]*\{presentation\.announcement\}/);
  assert.doesNotMatch(fetchStatusSource, /role="status"\s+aria-live="polite"\s+aria-label=\{presentation\.ariaLabel\}/);
});

test("idle compact preference is optional and persistent", () => {
  const values = new Map<string, string>();
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
  };
  assert.equal(readFetchStatusIdleCompact(storage), false);
  writeFetchStatusIdleCompact(true, storage);
  assert.equal(values.get(FETCH_STATUS_IDLE_COMPACT_KEY), "true");
  assert.equal(readFetchStatusIdleCompact(storage), true);
  writeFetchStatusIdleCompact(false, storage);
  assert.equal(readFetchStatusIdleCompact(storage), false);

  const denied = {
    getItem: () => { throw new Error("denied"); },
    setItem: () => { throw new Error("denied"); },
  };
  assert.equal(readFetchStatusIdleCompact(denied), false);
  assert.doesNotThrow(() => writeFetchStatusIdleCompact(true, denied));

  const blockedScope = {
    get localStorage(): never { throw new Error("blocked"); },
  };
  assert.equal(resolveFetchStatusPreferenceStorage(blockedScope), null);
  assert.equal(resolveFetchStatusPreferenceStorage(null), null);
  assert.equal(resolveFetchStatusPreferenceStorage({ localStorage: storage }), storage);
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
