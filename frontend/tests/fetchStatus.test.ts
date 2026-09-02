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
  fetchStatusWarningGroups,
  formatFetchRelativeTime,
  nextFetchStatusDelay,
  nextFetchTime,
  pollFetchStatus,
  shouldShowFetchStatusStrip,
} from "../src/lib/fetchStatus.js";
import type { FetchProgressStatus, FetchStatusResponse } from "../src/types/fetchStatus.js";

const fetchStatusSource = readFileSync(resolve(process.cwd(), "src/components/FetchStatus.tsx"), "utf8");

const activeStatus: FetchProgressStatus = {
  schema_version: 13,
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

const completedStatus: FetchProgressStatus = {
  ...activeStatus,
  phase: "complete",
  outcome: "succeeded",
  analyses: { ...activeStatus.analyses, logical_total: 79, completed: 79, running: 0, queued: 0 },
  pattern_phase: "completed",
  publication_phase: "completed",
  side_effect_phase: "completed",
  follow_up: {
    notifications: { state: "completed" },
    automatic_issues: { state: "skipped", reason: "not-configured" },
  },
};

const statusFixtures = {
  activeAnalysis: response("active", {
    ...activeStatus,
    analyses: { ...activeStatus.analyses, logical_total: 79, completed: 43, running: 2, queued: 34 },
  }),
  successfulIdleWatch: response("idle", {
    ...completedStatus,
    phase: "idle",
    pass_type: "lightweight-watch",
    next_watch_at: "2026-08-07T19:15:00Z",
    next_reconcile_at: "2026-08-07T20:00:00Z",
  }),
  analysisQualityWarning: response("completed", {
    ...completedStatus,
    analyses: { ...completedStatus.analyses, logical_total: 100, completed: 79, failed: 21 },
  }),
  patternRepairWarning: response("completed", {
    ...completedStatus,
    patterns: {
      eligible: 6, completed: 5, failed: 1, attempts: 6, retries: 0,
      repairs: 1, repair_succeeded: 0, repair_failed: 1, repair_failure_category: "schema", current: 5,
    },
  }),
  publicationFailure: response("failed", {
    ...activeStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "publication",
    pattern_phase: "completed",
    publication_phase: "failed",
  }),
  publishedFollowUpFailure: response("failed", {
    ...completedStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "side-effects",
    analyses: { ...completedStatus.analyses, logical_total: 100, completed: 79, failed: 21 },
    patterns: {
      eligible: 6, completed: 5, failed: 1, attempts: 6, retries: 0,
      repairs: 1, repair_succeeded: 0, repair_failed: 1, repair_failure_category: "schema", current: 5,
    },
    side_effect_phase: "failed",
    follow_up: {
      notifications: {
        state: "failed", code: "notification-delivery", summary: "Email notification delivery failed",
      },
      automatic_issues: { state: "skipped", reason: "not-configured" },
    },
  }),
  missingAutomaticTokens: response("completed", completedStatus),
  notificationFailure: response("failed", {
    ...completedStatus,
    phase: "failed",
    outcome: "failed",
    failure_category: "side-effects",
    side_effect_phase: "failed",
    follow_up: {
      notifications: {
        state: "failed", code: "notification-delivery", summary: "Email notification delivery failed",
      },
    },
  }),
  staleIdle: response("stale", {
    ...completedStatus,
    phase: "idle",
    next_watch_at: "2026-08-07T18:00:00Z",
  }),
  interruptedPrePublication: response("interrupted", {
    ...activeStatus,
    phase: "interrupted",
    outcome: "interrupted",
    failure_category: "interrupted",
    pattern_phase: "completed",
    publication_phase: "pending",
  }),
  interruptedPostPublication: response("interrupted", {
    ...completedStatus,
    phase: "interrupted",
    outcome: "interrupted",
    failure_category: "interrupted",
    side_effect_phase: "cancelled",
    follow_up: {
      notifications: { state: "completed" },
    },
  }),
};

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
    ["Follow-up", "Pending"],
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

test("representative refresh fixtures keep publication, quality, and follow-up semantics separate", () => {
  assert.equal(fetchStatusPresentation(statusFixtures.activeAnalysis)?.title, "Analyzing failures");
  assert.equal(fetchStatusPresentation(statusFixtures.successfulIdleWatch)?.title, "Up to date");

  assert.deepEqual(fetchStatusWarningGroups(statusFixtures.analysisQualityWarning.status!), [
    { label: "Analysis quality", items: ["21 analyses unavailable"] },
  ]);
  assert.deepEqual(fetchStatusWarningGroups(statusFixtures.patternRepairWarning.status!), [
    { label: "Pattern quality", items: ["1 pattern attempt failed", "1 repair failed because the response had an invalid schema"] },
  ]);

  const publicationFailure = fetchStatusPresentation(statusFixtures.publicationFailure);
  assert.equal(publicationFailure?.title, "Refresh failed");
  assert.equal(publicationFailure?.severity, "error");

  const missingTokens = statusFixtures.missingAutomaticTokens.status?.follow_up;
  assert.equal(missingTokens?.automatic_issues?.state, "skipped");
  assert.equal(missingTokens?.automatic_issues?.reason, "not-configured");
  assert.deepEqual(fetchStatusWarningGroups(statusFixtures.missingAutomaticTokens.status!), []);

  assert.deepEqual(fetchStatusWarningGroups(statusFixtures.notificationFailure.status!), [
    { label: "Follow-up", items: ["Email notification delivery failed"] },
  ]);
  assert.equal(fetchStatusPresentation(statusFixtures.staleIdle)?.title, "Status stale");
  assert.equal(fetchStatusPresentation(statusFixtures.interruptedPrePublication)?.title, "Refresh interrupted");
  const interruptedAfterPublish = fetchStatusPresentation(statusFixtures.interruptedPostPublication);
  assert.equal(interruptedAfterPublish?.title, "Dashboard updated with a follow-up warning");
  assert.equal(interruptedAfterPublish?.detail, "The latest dashboard is live, but follow-up work was interrupted.");
  assert.match(interruptedAfterPublish?.ariaLabel ?? "", /latest dashboard is live/i);
});

test("published follow-up failure keeps Publish complete and presents a warning", () => {
  const presentation = fetchStatusPresentation(statusFixtures.publishedFollowUpFailure);
  const stages = fetchStatusMacroStages(statusFixtures.publishedFollowUpFailure);

  assert.deepEqual(stages.map((stage) => [stage.label, stage.stateLabel]), [
    ["Fetch data", "Complete"],
    ["Analyze", "Complete"],
    ["Patterns", "Complete"],
    ["Publish", "Complete"],
    ["Follow-up", "Failed"],
  ]);
  assert.equal(presentation?.title, "Dashboard updated with a follow-up warning");
  assert.equal(presentation?.detail, "The latest dashboard is live, but email notification delivery failed.");
  assert.equal(presentation?.severity, "warning");
  assert.match(presentation?.ariaLabel ?? "", /latest dashboard is live/i);
  assert.doesNotMatch(presentation?.ariaLabel ?? "", /\.\./);
  assert.doesNotMatch(`${presentation?.title} ${presentation?.detail}`, /Publish failed|Refresh failed/);
  assert.deepEqual(fetchStatusWarningGroups(statusFixtures.publishedFollowUpFailure.status!), [
    { label: "Analysis quality", items: ["21 analyses unavailable"] },
    { label: "Pattern quality", items: ["1 pattern attempt failed", "1 repair failed because the response had an invalid schema"] },
    { label: "Follow-up", items: ["Email notification delivery failed"] },
  ]);
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
  assert.deepEqual(stages.map((stage) => stage.stateLabel), ["Complete", "Complete", "Complete", "Complete", "Complete"]);
  assert.equal(shouldShowFetchStatusStrip(staleIdle), true);
  assert.match(fetchStatusSource, /const completedPipeline = fetchStatusHasCompletedPipeline\(response\)/);
  assert.match(fetchStatusSource, /\{completedPipeline \? \([\s\S]*Next check/);
  assert.match(fetchStatusSource, /publishedThisPass \|\| completedPipeline/);
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

test("the status popover stays a fixed-height summary with no nested disclosure", () => {
  // The control is anchored in the navigation rail footer, roughly 90px from
  // the bottom of the viewport. MUI positions a Popover when it opens and does
  // not reposition it when its children grow, so anything that expands in
  // place pushes content past the bottom edge where it cannot be scrolled to.
  const controlIndex = fetchStatusSource.indexOf("export function FetchStatusControl");
  const detailsIndex = fetchStatusSource.indexOf("export function RefreshPipelineDetails");
  assert.ok(controlIndex > 0 && detailsIndex > controlIndex);
  const popoverSource = fetchStatusSource.slice(controlIndex, detailsIndex);

  assert.doesNotMatch(popoverSource, /<Collapse/);
  assert.doesNotMatch(popoverSource, /aria-expanded=\{(technicalOpen|debugOpen|historyOpen)\}/);
  assert.doesNotMatch(popoverSource, /Run ID|Pass ID|Engine version/);
  // It keeps the freshness answer, which is what a persistent control owes the
  // reader, and links to the rest.
  assert.match(popoverSource, /Last published/);
  assert.match(popoverSource, /Current pass began/);
  assert.match(popoverSource, /Last activity/);
  assert.match(popoverSource, /Next check/);
  assert.match(popoverSource, /Next full reconciliation/);
  assert.match(popoverSource, /published dashboard.*available/i);
  assert.match(popoverSource, /to="\/analysis-health#refresh-pipeline"/);
  assert.match(fetchStatusSource, /overflowX: "hidden"/);
});

test("refresh pipeline details keep the operator diagnostics off the popover", () => {
  const detailsSource = fetchStatusSource.slice(
    fetchStatusSource.indexOf("export function RefreshPipelineDetails"),
  );
  // The last-pass block ends where the copyable identifiers begin; several
  // labels below are legitimate there but were dropped from the summary rows.
  const lastPassSource = detailsSource.slice(0, detailsSource.indexOf("Debug identifiers"));

  for (const label of ["Timing", "Analysis", "Cache", "Patterns", "Follow-up", "Retries", "Failures and cancellations"]) {
    assert.match(lastPassSource, new RegExp(`label="${label}"`));
  }
  // These were deliberately dropped as noise and must not creep back in.
  for (const removed of [
    "Phase", "Pattern stage", "Publication stage", "Follow-up stage", "Phase began", "Last checked",
    "Compatible results", "Exact results reused", "Same-failure results reused", "Existing Tasks adopted",
    "New analyzer Tasks", "Task attempts", "Same-failure candidates", "Cache rejections", "Phase durations",
  ]) {
    assert.doesNotMatch(lastPassSource, new RegExp(`label="${removed}"`));
  }
  // Debug identifiers are what an operator pastes into a bug report, so they
  // survive the move and stay copyable.
  for (const id of ["Run ID", "Pass ID", "Engine version", "Follow-up code"]) {
    assert.match(detailsSource, new RegExp(id));
  }
  assert.match(detailsSource, /Recent refreshes/);
  // The component must only bail when there is genuinely no snapshot; an
  // unconditional null would silently empty the section.
  assert.match(detailsSource, /if \(!response \|\| !status\) return null;/);
  assert.doesNotMatch(detailsSource, /^\s*return null;\s*$/m);
  // Only the last three passes are shown, however the call is wrapped.
  assert.match(detailsSource, /history\s*\n?\s*\.slice\(-3\)/);
  assert.match(fetchStatusSource, /navigator\.clipboard\.writeText/);
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
  assert.match(fetchStatusSource, /minWidth: iconOnly \? 44 : \{ xs: 44, md: "auto" \}/);
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

test("the refresh pipeline section survives a missing snapshot and is reachable", () => {
  const page = readFileSync(resolve(process.cwd(), "src/pages/AnalysisHealthPage.tsx"), "utf8");
  const section = page.slice(page.indexOf("function RefreshPipelineSection"));

  // A null response means the endpoint is off for this viewer. A response with
  // no status means the pipeline has nothing to report, which is exactly when
  // an operator goes looking, so the section must stay visible and say so.
  assert.match(section, /if \(!response\) return null;/);
  assert.match(section, /response\.status \?/);
  assert.match(page, /No refresh snapshot is available/);
  // "missing" and "unavailable" mean different things to an operator trying to
  // tell a broken pipeline from one that has not run.
  assert.match(page, /Refresh progress could not be read/);
  assert.match(page, /case "unavailable":/);
  assert.match(page, /case "missing":/);
  assert.doesNotMatch(section, /if \(!response\?\.status\) return null;/);

  // The popover deep-links here, so the section scrolls itself once the trace
  // ledger above it has settled. Matches the Overview page's restore pattern.
  assert.match(section, /useLayoutEffect/);
  assert.match(section, /location\.hash !== "#refresh-pipeline"/);
  // Keyed on the history entry so following the link a second time re-scrolls.
  assert.match(section, /handledKey\.current === location\.key/);
  assert.doesNotMatch(section, /scrolled\.current/);
  assert.match(section, /requestAnimationFrame/);
  assert.match(section, /id="refresh-pipeline"/);
  // Focus lands on the heading, which shows a focus ring, rather than on a
  // wrapper with its outline suppressed.
  assert.match(section, /getElementById\("refresh-pipeline-heading"\)/);
  assert.match(section, /headingTabIndex=\{-1\}/);
  assert.doesNotMatch(section, /"&:focus": \{ outline: "none" \}/);
  assert.match(page, /<RefreshPipelineSection response=\{fetchStatus\} ready=\{!loading\} \/>/);
});
