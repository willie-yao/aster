import assert from "node:assert/strict";
import test from "node:test";
import {
  FETCH_STATUS_IDLE_COMPACT_KEY,
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
    logical_total: 61,
    accepted_cache_hits: 0,
    compatible_results_reused: 0,
    new_work: 0,
    stale_work: 0,
    cache_rejections: {
      missing: 0,
      expired: 0,
      tool_floor: 0,
      evidence_floor: 0,
      critique: 0,
      skill: 0,
      model: 0,
      prompt: 0,
      transient_persistence: 0,
      malformed: 0,
    },
    queued: 35,
    running: 2,
    completed: 23,
    failed: 1,
    cancelled: 0,
    task_attempts: 0,
    retries: 3,
    existing_tasks_adopted: 0,
    results_retrieved: 0,
    result_retrieval_retries: 0,
  },
  pattern_phase: "pending",
  publication_phase: "pending",
  side_effect_phase: "pending",
};

function response(state: FetchStatusResponse["state"], status: FetchProgressStatus = activeStatus): FetchStatusResponse {
  return { available: true, state, stale: state === "stale", status };
}

test("fetch status presentation covers active idle failed and stale states", () => {
  const active = fetchStatusPresentation(response("active"));
  assert.equal(active?.title, "Fetch in progress: Analysis");
  assert.equal(active?.detail, "24 of 61 analyses complete, 2 running, 35 queued, 3 retries");
  assert.equal(active?.determinateTotal, 61);
  assert.equal(active?.determinateCompleted, 24);
  assert.ok(active?.ariaLabel.includes("Fetch in progress"));
  const attempts = fetchStatusPresentation(response("active", {
    ...activeStatus,
    analyses: { ...activeStatus.analyses, task_attempts: 27, checkpoint_committed: true },
  }));
  assert.ok(attempts?.detail.includes("27 Task attempts"));
  assert.ok(attempts?.detail.includes("analysis checkpoint saved"));
  const oneRetry = fetchStatusPresentation(response("active", {
    ...activeStatus,
    analyses: { ...activeStatus.analyses, retries: 1 },
  }));
  assert.ok(oneRetry?.detail.includes("1 retry"));
  assert.ok(!oneRetry?.detail.includes("1 retries"));

  const patternRetry = fetchStatusPresentation(response("active", {
    ...activeStatus,
    phase: "patterns",
    patterns: {
      eligible: 2, completed: 1, failed: 1, attempts: 3, retries: 1, cache_hits: 1,
      repairs: 1, repair_failed: 1, repair_failure_category: "schema", failure_category: "ambiguous", retained: 1,
    },
  }));
  assert.ok(patternRetry?.detail.includes("3 pattern attempts"));
  assert.ok(patternRetry?.detail.includes("1 pattern retry"));
  assert.ok(patternRetry?.detail.includes("1 pattern cache hit"));
  assert.ok(patternRetry?.detail.includes("1 ambiguity repair"));
  assert.ok(patternRetry?.detail.includes("repair failure: invalid schema"));
  assert.ok(patternRetry?.detail.includes("1 last-known-good pattern"));
  assert.ok(patternRetry?.detail.includes("pattern failure: ambiguous response"));

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
  assert.equal(active?.label, "Analysis 24/61");
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
