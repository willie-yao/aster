import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import {
  attentionGroups,
  countLabel,
  currentPatternFailureStreak,
  disclosureLabel,
  mergeOverviewHistoryState,
  needsAttentionSummary,
  orderedDashboardBranches,
  patternEvidenceLabel,
  patternRecurrenceLabel,
  rankRecurringPatterns,
  overviewBranchFromParam,
  jobMatchesStatusFilter,
  overviewStatusFromParam,
  runStatusLabel,
  runChange,
  runDateLabel,
  readOverviewHistoryState,
  withOverviewFilters,
} from "../src/lib/dashboardOverview.js";
import { splitForMiddleTruncation } from "../src/lib/middleTruncate.js";
import type { BuildResult, FlakinessReport, JobSummary, PatternAnalysis, PatternRefreshStatus, RunSummary, TestFlakiness } from "../src/types/dashboard.js";
import type { Manifest } from "../src/types/manifest.js";

const { HealthPanel, JobHealthTable, AttentionRow, DisclosureButton, FeaturedPatternRow, NeedsAttention, ManifestContext, RunHistory, StatusFilterTiles, defaultTheme } = await withSSR(async (vite) => {
  const { HealthPanel } = (await vite.ssrLoadModule("/src/components/HealthPanel.tsx")) as {
    HealthPanel: (props: {
      jobs: JobSummary[];
      onFilterClick?: (status: string) => void;
      activeFilter?: string;
    }) => ReturnType<typeof createElement>;
  };
  const { JobHealthTable } = (await vite.ssrLoadModule("/src/components/JobHealthTable.tsx")) as {
    JobHealthTable: (props: { sections: Array<{ id: string; label?: string; jobs: JobSummary[] }> }) => ReturnType<typeof createElement>;
  };
  const {
    AttentionRow,
    DisclosureButton,
    FeaturedPatternRow,
    NeedsAttention,
  } = (await vite.ssrLoadModule("/src/components/NeedsAttention.tsx")) as {
    AttentionRow: (props: {
      to: string;
      destinationLabel: string;
      subject: string;
      summary: string;
      detail?: string;
      count?: string;
      signal?: string;
      statusColor?: "success" | "warning" | "error";
      muted?: boolean;
    }) => ReturnType<typeof createElement>;
    DisclosureButton: (props: {
      label: string;
      open: boolean;
      controls: string;
      onClick: () => void;
    }) => ReturnType<typeof createElement>;
    FeaturedPatternRow: (props: {
      pattern: PatternAnalysis;
      rank: number;
      prefix: string;
      refreshStatus?: PatternRefreshStatus;
      job?: JobSummary;
    }) => ReturnType<typeof createElement>;
    NeedsAttention: (props: {
      report: FlakinessReport;
      loading: boolean;
      error: string | null;
      jobsByID: Record<string, JobSummary>;
    }) => ReturnType<typeof createElement>;
  };
  const { ManifestContext } = (await vite.ssrLoadModule("/src/hooks/useManifest.ts")) as {
    ManifestContext: Context<Manifest | null>;
  };
  const { RunHistory } = (await vite.ssrLoadModule("/src/components/RunHistory.tsx")) as {
    RunHistory: (props: {
      runs: BuildResult[];
      selectedBuildId?: string;
      onSelect: (buildId: string) => void;
    }) => ReturnType<typeof createElement>;
  };
  const { StatusFilterTiles } = (await vite.ssrLoadModule("/src/components/StatusFilterTiles.tsx")) as {
    StatusFilterTiles: (props: {
      label: string;
      value: string;
      options: { value: string; label: string; tone: string; count?: number }[];
      onChange: (value: string) => void;
    }) => ReturnType<typeof createElement>;
  };
  const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
    defaultTheme: Theme;
  };
  return { HealthPanel, JobHealthTable, AttentionRow, DisclosureButton, FeaturedPatternRow, NeedsAttention, ManifestContext, RunHistory, StatusFilterTiles, defaultTheme };
});

function job(overrides: Partial<JobSummary> = {}): JobSummary {
  return {
    name: "capz-periodic-e2e-main",
    job_id: "capz-periodic-e2e-main",
    job_type: "periodic",
    repo: "kubernetes-sigs/cluster-api-provider-azure",
    tab_name: "capz-periodic-e2e-main",
    category: "CAPZ E2E",
    branch: "main",
    description: "Runs workload cluster creation tests.",
    minimum_interval: "1h",
    timeout: "2h",
    config_file: "config/jobs.yaml",
    current_status: "PASSING",
    overall_status: "PASSING",
    last_run: {
      build_id: "123",
      passed: true,
      timestamp: "2026-08-05T10:00:00Z",
      duration_seconds: 3600,
    },
    recent_runs: [
      { build_id: "123", passed: true, timestamp: "2026-08-05T10:00:00Z" },
      { build_id: "122", passed: false, timestamp: "2026-08-05T09:00:00Z" },
    ],
    pass_rate_recent: 0.5,
    ...overrides,
  };
}

function recurringPattern(
  jobID: string,
  buildsAnalyzed: number,
  sharedBuilds: string[],
  recoveryStreak: number,
): PatternAnalysis {
  return {
    id: `pattern-${jobID}`,
    subject: jobID,
    job_id: jobID,
    generated_at: "2026-08-28T18:47:11Z",
    builds_analyzed: buildsAnalyzed,
    causal_groups: [{ builds: sharedBuilds, root_cause: "same cause", confidence: "high" }],
    systemic: true,
    confidence: "high",
    shared_root_cause: "same cause",
    shared_builds: sharedBuilds,
    lifecycle: { state: "active", reason: "The recurring remediation remains unresolved.", recovery_streak: recoveryStreak },
    summary: "Repeated failure.",
  };
}

const desktopWidth = 1440;
const mobileWidth = 390;

// Without a window, MUI resolves useMediaQuery through ssrMatchMedia, so a
// render can pick the width the component sees. The ledger and the nav mount
// one layout per breakpoint, so a test has to say which one it is asserting.
function queryMatchesWidth(query: string, width: number): boolean {
  const min = /min-width:\s*([\d.]+)px/.exec(query);
  const max = /max-width:\s*([\d.]+)px/.exec(query);
  if (!min && !max) return false;
  if (min && width < Number(min[1])) return false;
  if (max && width > Number(max[1])) return false;
  return true;
}

function themeAtWidth(width: number): Theme {
  return {
    ...defaultTheme,
    components: {
      ...defaultTheme.components,
      MuiUseMediaQuery: {
        defaultProps: { ssrMatchMedia: (query: string) => ({ matches: queryMatchesWidth(query, width) }) },
      },
    },
  } as Theme;
}

function render(element: ReturnType<typeof createElement>, width = desktopWidth): string {
  return renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: themeAtWidth(width) },
      createElement(MemoryRouter, null, element),
    ),
  );
}

function testAlert(testName: string, jobID: string, buildID?: string): TestFlakiness {
  return {
    test_name: testName, job_name: jobID, job_id: jobID,
    total_runs: 7, failures: 5, passes: 2, flip_rate: 0.2, fail_rate: 5 / 7,
    consecutive_failures: 5, classification: "persistent",
    last_failure: buildID ? { build_id: buildID, timestamp: "", failure_message: "", error_hash: "" } : undefined,
  };
}

function alertReport(overrides: Partial<FlakinessReport>): FlakinessReport {
  return {
    generated_at: "", most_flaky: [], persistent_failures: [],
    recently_broken: [], build_failures: [], ...overrides,
  };
}

function renderAttention(report: FlakinessReport): string {
  const manifest: Manifest = {
    id: "example", name: "Example",
    testgrid: { dashboard: "example" }, storage: { provider: "gcs", bucket: "logs" },
    branding: { title: "Example", base_path: "/", site_url: "", source_repo: { owner: "example", name: "example" } },
  };
  return render(createElement(
    ManifestContext.Provider,
    { value: manifest },
    createElement(NeedsAttention, { report, loading: false, error: null, jobsByID: {} }),
  ));
}

test("health summary exposes counts and pressed filter state", () => {
  const jobs = [
    job(),
    job({ job_id: "flaky", name: "flaky", overall_status: "FLAKY" }),
    job({ job_id: "failing", name: "failing", overall_status: "FAILING" }),
  ];
  const html = render(createElement(HealthPanel, { jobs, activeFilter: "FLAKY", onFilterClick: () => undefined }));

  assert.match(html, /Reliability over the last 10 runs/);
  assert.match(html, /3 jobs/);
  assert.match(html, /aria-label="Healthy: 1 job, 33% of jobs\./);
  assert.match(html, /aria-label="Flaky: 1 job, 33% of jobs\./);
  assert.match(html, /mix of passing and failing results/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /aria-label="Unreliable: 1 job, 33% of jobs\./);
  assert.match(html, /33%.{0,400}> of jobs<\/span>/);
  assert.match(html, /inset 0 -3px 0/);
});

test("reliability buckets and latest-run failures are separate filters", () => {
  const latestFailedButReliable = job({ job_id: "a", current_status: "FAILING", overall_status: "PASSING" });
  const unreliableButPassing = job({ job_id: "b", current_status: "PASSING", overall_status: "FAILING" });
  assert.equal(jobMatchesStatusFilter(latestFailedButReliable, "HEALTHY"), true);
  assert.equal(jobMatchesStatusFilter(latestFailedButReliable, "LATEST_FAILED"), true);
  assert.equal(jobMatchesStatusFilter(latestFailedButReliable, "UNRELIABLE"), false);
  assert.equal(jobMatchesStatusFilter(unreliableButPassing, "UNRELIABLE"), true);
  assert.equal(jobMatchesStatusFilter(unreliableButPassing, "LATEST_FAILED"), false);
  assert.equal(runStatusLabel("FAILING"), "Failed");
  assert.equal(runStatusLabel("PASSING"), "Passed");
});

test("job names truncate in the middle so the suffix stays visible", () => {
  assert.deepEqual(splitForMiddleTruncation("main"), { head: "main", tail: "" });
  const name = "capz-periodic-e2e-v1beta1-release-1-26";
  const { head, tail } = splitForMiddleTruncation(name);
  assert.equal(head + tail, name);
  assert.match(tail, /release-1-26$/);
});

test("job health ledger keeps job and run links separate", () => {
  const html = render(createElement(JobHealthTable, { sections: [{ id: "capz-e2e", label: "CAPZ E2E", jobs: [job()] }] }));

  assert.match(html, /role="table"/);
  assert.match(html, /role="cell"[^>]*>[\s\S]*<h3[^>]*>CAPZ E2E<\/h3>/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main"/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main\?run=123"/);
  assert.match(html, /aria-label="Run 123, passed, Aug 5, 2026"/);
  assert.match(html, />Last 10 pass</);
  assert.match(html, />Latest run</);
  assert.match(html, />Passed</);
  assert.doesNotMatch(html, />Current</);
  assert.doesNotMatch(html, /<a\b[^>]*>(?:(?!<\/a>)[\s\S])*<a\b/);
});

test("run sparkline shows every configured run up to its cap, oldest first", () => {
  const runsOf = (count: number) =>
    Array.from({ length: count }, (_, i) => ({
      build_id: String(200 - i),
      passed: i % 2 === 0,
      timestamp: "2026-08-05T10:00:00Z",
    }));
  const sparklineRuns = (html: string) =>
    [...html.matchAll(/aria-label="Run (\d+), /g)].map((match) => match[1]);

  // One ledger layout is mounted per breakpoint, so each run appears once.
  const ledger = sparklineRuns(
    render(createElement(JobHealthTable, {
      sections: [{ id: "capz-e2e", jobs: [job({ recent_runs: runsOf(10) })] }],
    })),
  );
  assert.equal(ledger.length, 10);
  // Newest run is rendered last so the sparkline reads left to right.
  assert.deepEqual(ledger, ["191", "192", "193", "194", "195", "196", "197", "198", "199", "200"]);

  // A deeper history is truncated to the newest runs rather than overflowing.
  const capped = sparklineRuns(
    render(createElement(JobHealthTable, {
      sections: [{ id: "capz-e2e", jobs: [job({ recent_runs: runsOf(20) })] }],
    })),
  );
  assert.equal(capped.length, 12);
  assert.deepEqual(capped, ["189", "190", "191", "192", "193", "194", "195", "196", "197", "198", "199", "200"]);

  // The mobile layout carries the same runs, not a second copy alongside it.
  const mobile = sparklineRuns(
    render(createElement(JobHealthTable, {
      sections: [{ id: "capz-e2e", jobs: [job({ recent_runs: runsOf(10) })] }],
    }), mobileWidth),
  );
  assert.deepEqual(mobile, ledger);
});

test("job health ledger mounts one layout per breakpoint", () => {
  const sections = [{ id: "capz-e2e", label: "CAPZ E2E", jobs: [job()] }];
  const desktop = render(createElement(JobHealthTable, { sections }));
  const mobile = render(createElement(JobHealthTable, { sections }), mobileWidth);

  // Rendering both and hiding one with CSS doubles the overview DOM, so each
  // width mounts its own layout and nothing else.
  assert.match(desktop, /role="table"/);
  assert.doesNotMatch(desktop, /role="listitem"/);
  assert.match(mobile, /role="listitem"/);
  assert.doesNotMatch(mobile, /role="table"/);
  // The category heading is rendered once, not once per hidden layout.
  assert.equal(desktop.match(/<h3[^>]*>CAPZ E2E<\/h3>/g)?.length, 1);
  assert.equal(mobile.match(/<h3[^>]*>CAPZ E2E<\/h3>/g)?.length, 1);
});

test("run sparkline is one composite tab stop with arrow-key movement", () => {
  const runsOf = (count: number) =>
    Array.from({ length: count }, (_, i) => ({
      build_id: String(200 - i),
      passed: i % 2 === 0,
      timestamp: "2026-08-05T10:00:00Z",
    }));
  const strip = (width: number) => {
    const html = render(createElement(JobHealthTable, {
      sections: [{ id: "capz-e2e", jobs: [job({ recent_runs: runsOf(10) })] }],
    }), width);
    return [...html.matchAll(/<a\b[^>]*aria-label="Run \d+,[^"]*"[^>]*>/g)].map((m) => m[0]);
  };

  // Each strip exposes exactly one tabbable run, so a ledger of dozens of jobs
  // costs one tab stop per strip instead of hundreds.
  for (const width of [desktopWidth, mobileWidth]) {
    const runLinks = strip(width);
    assert.equal(runLinks.length, 10);
    assert.equal(runLinks.filter((link) => /tabindex="0"/.test(link)).length, 1);
    assert.equal(runLinks.filter((link) => /tabindex="-1"/.test(link)).length, 9);
  }
  assert.match(render(createElement(JobHealthTable, {
    sections: [{ id: "capz-e2e", jobs: [job()] }],
  })), /aria-label="Recent runs"/);

  const sparkline = readFileSync(resolve(process.cwd(), "src/components/Sparkline.tsx"), "utf8");
  for (const key of ["ArrowRight", "ArrowLeft", "Home", "End"]) {
    assert.match(sparkline, new RegExp(`"${key}"`), `${key} moves focus within the strip`);
  }
});

test("overview columns reserve a full-size target for every run the sparkline caps at", () => {
  const source = (path: string) => readFileSync(resolve(process.cwd(), path), "utf8");
  const sparkline = source("src/components/Sparkline.tsx");
  const ledger = source("src/components/JobHealthTable.tsx");
  const attention = source("src/components/NeedsAttention.tsx");
  const number = (pattern: RegExp, text: string) => Number(pattern.exec(text)?.[1]);

  const maxRuns = number(/export const sparklineMaxRuns = (\d+)/, sparkline);
  const cell = number(/const desktopCell = (\d+)/, sparkline);
  assert.ok(maxRuns >= 10, "projects configure at least 10 builds");
  // Each dot is its own link, and they sit edge to edge, so the cell width is
  // the spacing WCAG 2.5.8 measures between adjacent targets.
  assert.ok(cell >= 24, "run links keep the minimum pointer target size");

  // Every row shares one grid template, so the columns must reserve the cap.
  const runsTrack = (declaration: RegExp) =>
    number(/^minmax\([^)]*\)\s+\d+px\s+(\d+)px/, declaration.exec(ledger)![1]);
  const reserved = maxRuns * cell;
  assert.ok(runsTrack(/const compactColumns = "([^"]+)"/) >= reserved);
  assert.ok(runsTrack(/const wideColumns = "([^"]+)"/) >= reserved);
  // The attention evidence column also carries pr: 1.5 (12px) of padding.
  const evidence = number(/gridTemplateColumns: "minmax\(0, 1fr\) (\d+)px"/, attention);
  assert.ok(evidence >= reserved + 12);
});

test("detail run controls include result build and date context", () => {
  const run: BuildResult = {
    build_id: "123",
    job_name: "capz-periodic-e2e-main",
    started: "2026-08-05T10:00:00Z",
    finished: "2026-08-05T11:00:00Z",
    passed: false,
    result: "FAILURE",
    duration_seconds: 3600,
    commit: "abcdef12",
    prow_url: "https://prow.example/123",
    web_url: "https://storage.example/123",
    build_log_url: "https://storage.example/123/build-log.txt",
    test_cases: [],
    tests_total: 1,
    tests_passed: 0,
    tests_failed: 1,
    tests_skipped: 0,
  };
  const html = render(createElement(RunHistory, {
    runs: [run],
    selectedBuildId: "123",
    onSelect: () => undefined,
  }));

  assert.match(html, /aria-label="#123 · Failed · Aug 5, 2026"/);
  assert.match(html, />8\/5</);
});

test("overview count labels place dynamic counts in Needs attention", () => {
  assert.equal(countLabel(1, "job"), "1 job");
  assert.equal(countLabel(2, "job"), "2 jobs");
  assert.equal(needsAttentionSummary(5, 12, false, false), "5 recurring patterns · 12 test alerts");
  assert.equal(needsAttentionSummary(null, null, true, false), "recurring patterns loading · test alerts loading");
  assert.equal(needsAttentionSummary(null, null, false, true), "recurring patterns unavailable · test alerts unavailable");
  assert.equal(needsAttentionSummary(5, 12, false, false, 0), "5 recurring patterns · 12 test alerts");
  assert.equal(needsAttentionSummary(1, 0, false, false, 2), "1 recurring pattern · 0 test alerts · 2 shared PR failures");
});

test("recurring pattern ranking leads with an active same-cause streak", () => {
  const patterns = [
    recurringPattern("first", 5, ["first-2", "first-1"], 1),
    recurringPattern("second", 4, ["second-2", "second-1"], 2),
    recurringPattern("third", 3, ["third-3", "third-2", "third-1"], 2),
    recurringPattern("fourth", 3, ["fourth-3", "fourth-2", "fourth-1"], 0),
  ];
  const jobs = Object.fromEntries(patterns.map((pattern) => {
    const jobID = pattern.job_id ?? pattern.subject;
    const active = jobID === "fourth";
    return [jobID, job({
      job_id: jobID,
      name: jobID,
      current_status: active ? "FAILING" : "PASSING",
      overall_status: "FLAKY",
      recent_runs: active
        ? [
          { build_id: "pending", passed: false, result: "PENDING", timestamp: "2026-08-28T18:00:00Z" },
          { build_id: "fourth-3", passed: false, result: "FAILURE", timestamp: "2026-08-28T17:00:00Z" },
          { build_id: "fourth-2", passed: false, result: "FAILURE", timestamp: "2026-08-28T16:00:00Z" },
          { build_id: "fourth-1", passed: false, result: "FAILURE", timestamp: "2026-08-28T15:00:00Z" },
        ]
        : [{ build_id: `${jobID}-pass`, passed: true, result: "SUCCESS", timestamp: "2026-08-28T18:00:00Z" }],
    })];
  }));

  assert.equal(currentPatternFailureStreak(patterns[3], jobs.fourth), 3);
  assert.deepEqual(
    rankRecurringPatterns(patterns, jobs).map((pattern) => pattern.job_id),
    ["fourth", "first", "third", "second"],
  );
  assert.deepEqual(patterns.map((pattern) => pattern.job_id), ["first", "second", "third", "fourth"]);
});

test("missing recovery metadata does not imply a recovered pattern", () => {
  const unknown = recurringPattern("unknown", 2, ["unknown-2", "unknown-1"], 0);
  delete unknown.lifecycle;
  const recovering = recurringPattern("recovering", 5, ["recovering-2", "recovering-1"], 1);
  const jobs = {
    unknown: job({ job_id: "unknown", recent_runs: [{ build_id: "pass", passed: true, result: "SUCCESS", timestamp: "2026-08-28T18:00:00Z" }] }),
    recovering: job({ job_id: "recovering", recent_runs: [{ build_id: "pass", passed: true, result: "SUCCESS", timestamp: "2026-08-28T18:00:00Z" }] }),
  };

  assert.deepEqual(
    rankRecurringPatterns([recovering, unknown], jobs).map((pattern) => pattern.job_id),
    ["unknown", "recovering"],
  );
});

test("recurring pattern evidence says how many analyzed failures share the cause", () => {
  const partial = recurringPattern("partial", 5, ["2", "1"], 0);
  const active = recurringPattern("active", 3, ["3", "2", "1"], 0);
  const text = (pattern: Parameters<typeof patternEvidenceLabel>[0]) => patternEvidenceLabel(pattern).text;

  assert.equal(text(partial), "This cause in 2 of 5 failed runs");
  assert.equal(text(active), "Same cause in all 3 failed runs");
  assert.equal(text({ ...partial, shared_builds: [], causal_groups: [] }), "5 failed runs analyzed");
  assert.equal(text({ ...partial, builds_analyzed: 1, shared_builds: ["1"], causal_groups: [] }), "This cause in 1 of 1 failed run");
  assert.equal(text({ ...partial, builds_analyzed: 1, shared_builds: [], causal_groups: [] }), "1 failed run analyzed");
  assert.equal(text({ ...partial, confidence: "medium" }), "This cause in 2 of 5 failed runs · medium confidence");
  assert.equal(text({ ...partial, confidence: "low" }), "This cause in 2 of 5 failed runs · low confidence");
});

test("recurring pattern evidence flags failures newer than the analysis", () => {
  const pattern = recurringPattern("main", 3, ["old-2", "old-1"], 1);
  const run = (build_id: string, passed: boolean, result = passed ? "SUCCESS" : "FAILURE") =>
    ({ build_id, passed, result, timestamp: "2026-10-01T00:00:00Z" }) as RunSummary;

  // The newest failure is one the analysis saw, so the cause covers it.
  const covered = patternEvidenceLabel(pattern, [run("old-2", false), run("pass", true), run("old-1", false)]);
  assert.deepEqual([covered.text, covered.unanalyzed], ["This cause in 2 of 3 failed runs", 0]);

  // Failures after the newest analyzed run are counted; older ones and pending runs are not.
  const newer = patternEvidenceLabel(pattern, [run("pending", false, "PENDING"), run("new", false), run("old-2", false), run("older", false)]);
  assert.deepEqual([newer.text, newer.unanalyzed], ["1 recent failure not analyzed", 1]);

  // An unclassified build was analyzed even though no cause was assigned to it.
  const unclassified = patternEvidenceLabel({ ...pattern, unclassified_builds: ["unsure"] }, [run("unsure", false), run("old-2", false)]);
  assert.deepEqual([unclassified.text, unclassified.unanalyzed], ["This cause in 2 of 3 failed runs", 0]);

  // With none of the analyzed runs in view, every failure in view came after them.
  const retained = patternEvidenceLabel(pattern, [run("b", false), run("a", false), run("p", true)]);
  assert.deepEqual([retained.text, retained.unanalyzed], ["2 recent failures not analyzed", 2]);
  assert.match(retained.detail, /^2 recent failures not analyzed\. This cause in 2 of 3 failed runs/);
});

test("recurring pattern evidence dates a retained analysis in its detail", () => {
  const pattern = recurringPattern("main", 3, ["old-2", "old-1"], 1);
  const refresh: PatternRefreshStatus = {
    state: "retained", evidence_available: false, last_successful_at: "2026-09-28T23:30:00Z",
  };
  const detail = (status: PatternRefreshStatus, p = pattern) => patternEvidenceLabel(p, [], status).detail;
  assert.equal(patternEvidenceLabel(pattern, [], refresh).text, "This cause in 2 of 3 failed runs");
  assert.equal(detail(refresh), "This cause in 2 of 3 failed runs · analyzed Sep 28");
  assert.equal(detail(refresh, { ...pattern, confidence: "medium" }), "This cause in 2 of 3 failed runs · medium confidence · analyzed Sep 28");
  assert.equal(detail({ ...refresh, state: "current" }), "This cause in 2 of 3 failed runs");
  assert.equal(detail({ ...refresh, last_successful_at: undefined }), "This cause in 2 of 3 failed runs · from an earlier refresh");
  assert.equal(detail({ ...refresh, last_successful_at: "invalid" }), "This cause in 2 of 3 failed runs · from an earlier refresh");
});

test("latest recurrence label tolerates missing timestamps without inventing a date", () => {
  assert.equal(patternRecurrenceLabel({}), null);
  assert.equal(patternRecurrenceLabel({ lifecycle: { state: "active", reason: "" } }), null);
  const lifecycle = { state: "active" as const, reason: "", latest_recurrence_build_id: "new" };
  assert.equal(patternRecurrenceLabel({ lifecycle }), "Latest recurrence");
  assert.equal(patternRecurrenceLabel({ lifecycle: { ...lifecycle, latest_recurrence_started: "invalid" } }), "Latest recurrence");
  assert.equal(patternRecurrenceLabel({
    lifecycle: { ...lifecycle, latest_recurrence_started: "2026-10-19T00:30:00Z" },
  }), "Recurred Oct 19");
});

test("featured pattern combines passing recurrence and recovery above its evidence line", () => {
  const pattern = recurringPattern("main", 3, ["old-2", "old-1"], 1);
  pattern.lifecycle = {
    state: "active", reason: "recent recurrence", recovery_streak: 1,
    latest_recurrence_build_id: "oct-19", latest_recurrence_started: "2026-10-19T12:00:00Z",
  };
  const html = render(createElement(FeaturedPatternRow, {
    pattern, rank: 1, prefix: "", job: job({ job_id: "main" }),
    refreshStatus: { state: "retained", evidence_available: false, last_successful_at: "2026-09-28T12:00:00Z" },
  }));
  assert.match(html, /aria-label="Passing · recurred Oct 19 · 1 of 3 passes to clear"/);
  const content = html.replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, "");
  assert.match(content, />Passing · <a[^>]*href="\/job\/main\?run=oct-19"[^>]*>recurred Oct 19<\/a> · 1\/3 passes to clear<\/div>/);
  // The fixture's failed run 122 is not one the pattern analyzed.
  assert.match(content, />1 recent failure not analyzed</);
  assert.match(html, /title="1 recent failure not analyzed\. This cause in 2 of 3 failed runs · analyzed Sep 28"/);
  assert.doesNotMatch(html, /high confidence|earlier window|Last successful refresh|Passing now/);
  assert.doesNotMatch(html, /<a\b[^>]*>(?:(?!<\/a>)[\s\S])*<a\b/);
});

test("recovery progress follows the active lifecycle below the recovery threshold", () => {
  const pattern = recurringPattern("main", 3, ["2", "1"], 2);
  const row = (value: PatternAnalysis, currentJob: JobSummary | undefined = job()) =>
    render(createElement(FeaturedPatternRow, { pattern: value, rank: 1, prefix: "", job: currentJob }));
  assert.match(row(pattern), /aria-label="Passing · 2 of 3 passes to clear"/);
  assert.match(row(pattern), />Passing · 2\/3 passes to clear</);
  assert.match(row(pattern, job({ current_status: "RUNNING" })), />Running · 2\/3 passes to clear</);
  assert.doesNotMatch(row(recurringPattern("main", 3, ["2", "1"], 0)), /passes to clear|\/3 passes/);
  assert.doesNotMatch(row(recurringPattern("main", 3, ["2", "1"], 3)), /passes to clear|\/3 passes/);
  assert.doesNotMatch(row({ ...pattern, lifecycle: { state: "recovered", reason: "", recovery_streak: 3 } }), /passes to clear|\/3 passes/);
  const unknown = recurringPattern("main", 3, ["2", "1"], 0);
  assert.match(row(unknown, job({ current_status: "UNKNOWN" })), />Current status unknown</);
  assert.match(render(createElement(FeaturedPatternRow, { pattern: unknown, rank: 1, prefix: "" })), />Recurring</);
});

test("covered alerts require the same job and exact last failure in displayed active evidence", () => {
  const pattern = recurringPattern("main", 3, ["shared"], 0);
  pattern.causal_groups = [{ builds: ["causal"], root_cause: "cause", confidence: "high" }];
  const report = alertReport({
    recently_broken: [
      testAlert("covered-shared", "main", "shared"),
      testAlert("covered-causal", "main", "causal"),
      testAlert("other-job", "other", "shared"),
      testAlert("other-run", "main", "different"),
      testAlert("missing-failure", "main"),
    ],
  });
  const names = (patterns: PatternAnalysis[]) => attentionGroups(report, undefined, patterns)
    .flatMap((group) => group.items.map((item) => item.test_name));
  assert.deepEqual(names([pattern]), ["other-job", "other-run", "missing-failure"]);
  assert.equal(names([]).length, 5);
  assert.equal(names([{ ...pattern, lifecycle: { state: "recovered", reason: "" } }]).length, 5);
  assert.deepEqual(names([{ ...pattern, lifecycle: undefined }]), ["other-job", "other-run", "missing-failure"]);
});

test("covered alerts are removed from all groups before the shared item limit", () => {
  const pattern = recurringPattern("main", 7, ["failure"], 0);
  const covered = testAlert("covered", "main", "failure");
  const uncovered = Array.from({ length: 10 }, (_, index) => testAlert(`uncovered-${index}`, "other", "failure"));
  const report = alertReport({
    recurring_patterns: [pattern],
    recently_broken: Array.from({ length: 10 }, (_, index) => ({ ...covered, test_name: `covered-${index}` })),
    persistent_failures: [covered, ...uncovered],
    most_flaky: [covered],
    low_pass_rate: [{ ...covered, pass_rate: 0.5, window_runs: 10 }],
  });
  const groups = attentionGroups(report, undefined, [pattern]);
  assert.deepEqual(groups.map((group) => group.kind), ["persistent"]);
  assert.deepEqual(groups[0].items.map((item) => item.test_name), uncovered.map((item) => item.test_name));
  assert.match(renderAttention(report), /1 recurring pattern · 10 test alerts/);
  const onlyCovered = { ...report, persistent_failures: [covered] };
  assert.deepEqual(attentionGroups(onlyCovered, undefined, [pattern]), []);
  const html = renderAttention(onlyCovered);
  assert.match(html, /1 recurring pattern · 0 test alerts/);
  assert.doesNotMatch(html, /aria-label="Persistent failures"|aria-label="Flaky tests"|aria-label="Low pass rate"/);
});

test("patterns outside the displayed recurring list do not hide test alerts", () => {
  const patterns = Array.from({ length: 5 }, (_, index) => recurringPattern(`job-${index}`, 3, ["2", "1"], 0));
  const unlisted = recurringPattern("zz-unlisted", 3, ["2", "1"], 0);
  const recovered = recurringPattern("recovered", 3, ["2", "1"], 3);
  recovered.lifecycle = { state: "recovered", reason: "", recovery_streak: 3 };
  const html = renderAttention(alertReport({
    recurring_patterns: [...patterns, unlisted, recovered],
    recently_broken: [
      testAlert("covered-alert", "job-0", "2"),
      testAlert("unlisted-alert", "zz-unlisted", "2"),
      testAlert("recovered-alert", "recovered", "2"),
    ],
  }));
  assert.match(html, /5 recurring patterns · 2 test alerts/);
  assert.match(html, />Unlisted-alert</);
  assert.match(html, />Recovered-alert</);
  assert.doesNotMatch(html, />Covered-alert</);
});

test("ranking recognizes the newest retained recurrence without combining distinct causal groups", () => {
  const pattern = recurringPattern("main", 3, ["old-2", "old-1"], 0);
  pattern.lifecycle = { state: "active", reason: "", latest_recurrence_build_id: "new" };
  const current = job({
    recent_runs: [
      { build_id: "pending", passed: false, result: "PENDING", timestamp: "" },
      { build_id: "new", passed: false, result: "FAILURE", timestamp: "" },
      { build_id: "old-2", passed: false, result: "FAILURE", timestamp: "" },
    ],
  });
  assert.equal(currentPatternFailureStreak(pattern, current), 1);
  assert.equal(currentPatternFailureStreak(pattern, {
    recent_runs: [{ build_id: "pass", passed: true, result: "SUCCESS", timestamp: "" }, ...current.recent_runs],
  }), 0);
});

test("disclosure labels pluralize and expose expansion state", () => {
  assert.equal(
    disclosureLabel(false, 2, "additional recurring pattern", "additional recurring patterns"),
    "Show 2 additional recurring patterns",
  );
  assert.equal(
    disclosureLabel(false, 1, "additional persistent failure", "additional persistent failures"),
    "Show 1 additional persistent failure",
  );

  const closed = render(createElement(DisclosureButton, {
    label: disclosureLabel(false, 2, "additional recurring pattern", "additional recurring patterns"),
    open: false,
    controls: "additional-recurring-patterns",
    onClick: () => undefined,
  }));
  const open = render(createElement(DisclosureButton, {
    label: disclosureLabel(true, 2, "additional recurring pattern", "additional recurring patterns"),
    open: true,
    controls: "additional-recurring-patterns",
    onClick: () => undefined,
  }));

  assert.match(closed, /aria-expanded="false"/);
  assert.match(closed, /aria-controls="additional-recurring-patterns"/);
  assert.match(closed, />Show 2 additional recurring patterns</);
  assert.match(closed, /rotate\(-90deg\)/);
  assert.match(open, /aria-expanded="true"/);
  assert.match(open, />Hide 2 additional recurring patterns</);
  assert.match(open, /rotate\(0deg\)/);
});

test("attention rows use one full-row destination link", () => {
  const html = render(createElement(AttentionRow, {
    to: "/job/capz-periodic-e2e-main/test/example?run=123",
    destinationLabel: "Open latest test run for example in capz-periodic-e2e-main",
    subject: "capz-periodic-e2e-main",
    summary: "example",
    detail: "timed out waiting for cluster",
    count: "3 consecutive failures",
    signal: "Failing",
    statusColor: "error",
  }));

  assert.equal((html.match(/<a\b/g) ?? []).length, 1);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main\/test\/example\?run=123"/);
  assert.match(html, /aria-label="Open latest test run for example in capz-periodic-e2e-main"/);
  assert.match(html, /capz-periodic-e2e-main/);
  assert.match(html, /timed out waiting for cluster/);
  assert.match(html, /3 consecutive failures/);
  assert.match(html, />Failing</);
});

test("featured recurring pattern exposes a current same-cause streak", () => {
  const pattern = recurringPattern("active", 3, ["3", "2", "1"], 0);
  const activeJob = job({
    job_id: "active",
    name: "active",
    current_status: "FAILING",
    overall_status: "FLAKY",
    recent_runs: [
      { build_id: "3", passed: false, result: "FAILURE", timestamp: "2026-08-28T17:00:00Z" },
      { build_id: "2", passed: false, result: "FAILURE", timestamp: "2026-08-28T16:00:00Z" },
      { build_id: "1", passed: false, result: "FAILURE", timestamp: "2026-08-28T15:00:00Z" },
    ],
  });
  const html = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    job: activeJob,
  }));

  assert.match(html, />Failing · 3 in a row</);
  assert.match(html, />Same cause in all 3 failed runs</);
  assert.doesNotMatch(html, /high confidence/);
  assert.doesNotMatch(html, />Flaky</);

  const running = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    job: {
      ...activeJob,
      current_status: "RUNNING",
      recent_runs: [
        { build_id: "pending", passed: false, result: "PENDING", timestamp: "2026-08-28T18:00:00Z" },
        ...activeJob.recent_runs,
      ],
    },
  }));
  assert.match(running, /Running · after 3 same-cause failures/);
  assert.doesNotMatch(running, />Failing/);
});

test("featured recurring pattern keeps current failure separate from causal grouping", () => {
  const pattern = recurringPattern("mixed", 4, ["3", "2", "1"], 0);
  pattern.causal_groups?.push({
    builds: ["4"],
    root_cause: "latest cause is not confirmed",
    confidence: "medium",
  });
  const failingJob = job({
    job_id: "mixed",
    name: "mixed",
    current_status: "FAILING",
    overall_status: "FLAKY",
    recent_runs: [
      { build_id: "4", passed: false, result: "FAILURE", timestamp: "2026-08-31T07:00:00Z" },
      { build_id: "3", passed: false, result: "FAILURE", timestamp: "2026-08-28T05:00:00Z" },
      { build_id: "2", passed: false, result: "FAILURE", timestamp: "2026-08-25T03:00:00Z" },
      { build_id: "1", passed: false, result: "FAILURE", timestamp: "2026-08-22T02:00:00Z" },
    ],
  });

  assert.equal(currentPatternFailureStreak(pattern, failingJob), 0);
  const failing = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    job: failingJob,
  }));
  assert.match(failing, />Failing</);
  assert.match(failing, />This cause in 3 of 4 failed runs</);
  assert.doesNotMatch(failing, /in a row/);
  assert.doesNotMatch(failing, />Flaky</);

  const passing = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    job: { ...failingJob, current_status: "PASSING" },
  }));
  assert.match(passing, />Passing</);
  assert.doesNotMatch(passing, />Flaky/);
});

test("featured analysis link precedes separate recent-run links", () => {
  const recurring: PatternAnalysis = {
    id: "pattern-1",
    subject: "capz-periodic-e2e-main",
    job_id: "capz-periodic-e2e-main",
    generated_at: "2026-08-05T10:00:00Z",
    builds_analyzed: 4,
    systemic: true,
    confidence: "high",
    shared_root_cause: "Cluster reconciliation waits for an unavailable dependency.",
    summary: "Repeated reconciliation timeout.",
  };
  const html = render(createElement(FeaturedPatternRow, {
    pattern: recurring,
    rank: 1,
    prefix: "",
    job: job({ overall_status: "FAILING" }),
  }));
  const destinations = [...html.matchAll(/<a\b[^>]*href="([^"]+)"/g)].map((match) => match[1]);

  assert.equal(destinations[0], "/job/capz-periodic-e2e-main");
  // The run strip, oldest first, then the caption's first failing run and
  // last passing run.
  assert.deepEqual(destinations.slice(1), [
    "/job/capz-periodic-e2e-main?run=122",
    "/job/capz-periodic-e2e-main?run=123",
    "/job/capz-periodic-e2e-main?run=123",
    "/job/capz-periodic-e2e-main?run=122",
  ]);
  assert.match(html, /aria-label="View analysis for capz-periodic-e2e-main"/);
  assert.match(html, />View analysis →</);
  assert.doesNotMatch(html, /<a\b[^>]*>(?:(?!<\/a>)[\s\S])*<a\b/);
  assert.doesNotMatch(html, /tabindex="[1-9]/);
});

test("overview filters use canonical query parameters", () => {
  assert.equal(overviewStatusFromParam("flaky"), "FLAKY");
  assert.equal(overviewStatusFromParam("unknown"), "ALL");
  assert.equal(overviewStatusFromParam("passing"), "ALL");
  assert.equal(overviewStatusFromParam("latest-failed"), "LATEST_FAILED");
  assert.equal(overviewBranchFromParam("main", ["main", "release-1.26"]), "main");
  assert.equal(overviewBranchFromParam("release-1.25", ["main"]), "ALL");

  const filtered = withOverviewFilters(
    new URLSearchParams("trace=1&status=healthy&branch=release-1.24"),
    "FLAKY",
    "main",
  );
  assert.equal(filtered.toString(), "trace=1&status=flaky&branch=main");
  assert.equal(withOverviewFilters(filtered, "ALL", "ALL").toString(), "trace=1");
  assert.equal(withOverviewFilters(filtered, "LATEST_FAILED", "main").toString(), "trace=1&status=latest-failed&branch=main");
});

test("overview history state preserves disclosures and scroll per entry", () => {
  const merged = mergeOverviewHistoryState(
    { key: "overview", idx: 2, usr: { caller: "kept" } },
    {
      additionalOpen: true,
      expandedGroups: { "Persistent failures": true },
      scrollY: 712,
    },
  );
  assert.deepEqual(merged, {
    key: "overview",
    idx: 2,
    usr: {
      caller: "kept",
      overview: {
        additionalOpen: true,
        resolvedOpen: false,
        expandedGroups: { "Persistent failures": true },
        scrollY: 712,
      },
    },
  });
  assert.deepEqual(readOverviewHistoryState(merged), {
    additionalOpen: true,
    resolvedOpen: false,
    expandedGroups: { "Persistent failures": true },
    scrollY: 712,
  });
  assert.deepEqual(readOverviewHistoryState({ usr: { overview: { scrollY: -5, expandedGroups: { valid: true, invalid: "yes" } } } }), {
    additionalOpen: false,
    resolvedOpen: false,
    expandedGroups: { valid: true },
    scrollY: 0,
  });
});

test("dashboard branches keep main first and release branches newest first", () => {
  const branches = orderedDashboardBranches([
    job({ branch: "release-1.24" }),
    job({ branch: "main" }),
    job({ branch: "release-1.26" }),
    job({ branch: "release-1.25" }),
  ]);
  assert.deepEqual(branches, ["main", "release-1.26", "release-1.25", "release-1.24"]);
});

test("overview source uses ledger rows without nested panel scrolling", () => {
  const dashboard = readFileSync(resolve(process.cwd(), "src/pages/DashboardPage.tsx"), "utf8");
  const attention = readFileSync(resolve(process.cwd(), "src/components/NeedsAttention.tsx"), "utf8");
  const filters = readFileSync(resolve(process.cwd(), "src/components/OverviewFilters.tsx"), "utf8");
  const health = readFileSync(resolve(process.cwd(), "src/components/HealthPanel.tsx"), "utf8");
  const ledger = readFileSync(resolve(process.cwd(), "src/components/JobHealthTable.tsx"), "utf8");
  const search = readFileSync(resolve(process.cwd(), "src/components/SearchBar.tsx"), "utf8");
  const sparkline = readFileSync(resolve(process.cwd(), "src/components/Sparkline.tsx"), "utf8");
  const overviewTheme = readFileSync(resolve(process.cwd(), "src/theme/overview.ts"), "utf8");
  const sectionNav = readFileSync(resolve(process.cwd(), "src/components/OverviewSectionNav.tsx"), "utf8");

  assert.match(dashboard, /<JobHealthTable sections=/);
  assert.doesNotMatch(dashboard, /JobCard/);
  assert.doesNotMatch(dashboard, /HealthDonut/);
  assert.doesNotMatch(attention, /overflowY:\s*"auto"/);
  assert.match(attention, /jobPath\(pattern\.job_id/);
  assert.match(attention, /testRunPath\(item\.job_id, item\.test_name, item\.last_failure\.build_id\)/);
  assert.match(attention, /"additional recurring pattern"/);
  assert.match(attention, /additional\.map\(\(pattern, index\)[\s\S]*<FeaturedPatternRow/);
  assert.match(attention, /"resolved failures"/);
  assert.match(attention, /No active test alerts/);
  assert.match(attention, /No published test-level or recurring-pattern alerts need attention/);
  assert.match(attention, /const hasActiveItems = recurring\.length > 0 \|\| groups\.length > 0/);
  assert.match(attention, /const noActiveAlerts = Boolean\(report && !hasActiveItems\)/);
  assert.match(attention, /report && \(resolvedPatterns\.length > 0 \|\| unlisted\.length > 0 \|\| unlistedCauses\.length > 0\)/);
  assert.doesNotMatch(attention, /const allClear/);
  assert.doesNotMatch(attention, /New regressions/);
  assert.match(attention, /color: lead \? "error\.main" : "text\.secondary"/);
  assert.match(attention, /fontWeight: lead \? 700 : 600/);
  assert.doesNotMatch(attention, /background: "transparent"/);
  assert.match(attention, /<DisclosureButton[\s\S]*<Collapse id=\{controls\}/);
  assert.match(dashboard, />\s*Test Health Overview\s*</);
  assert.doesNotMatch(dashboard, /Incident briefing/);
  assert.doesNotMatch(dashboard, /failingJobs/);
  assert.match(attention, /needsAttentionSummary\(/);
  assert.match(attention, /destinationLabel=/);
  assert.match(attention, /data-featured-analysis-link/);
  assert.match(attention, /fontSize: "18px"/);
  assert.match(attention, /maxInlineSize: "56ch"/);
  assert.match(attention, /persistOverviewHistoryState/);
  assert.match(attention, /scrollMarginTop: \{ xs: "128px", lg: "72px" \}/);
  assert.doesNotMatch(attention, /fontSize: lead \? "24px" : "16px"/);
  assert.match(filters, /minHeight: 44/);
  assert.doesNotMatch(filters, />\s*Reliability\s*</);
  assert.match(filters, /<StatusFilterTiles\s+label="Job status filter"/);
  assert.match(filters, /height: 44/);
  assert.match(health, /onFilterClick\?\.\(active \? "ALL" : row\.status\)/);
  assert.match(health, /borderLeft: "1px solid"/);
  assert.match(health, /% of jobs|> of jobs</);
  assert.match(health, /mix of passing and failing results/);
  assert.doesNotMatch(health, /borderRight:/);
  assert.match(search, /width: 44[\s\S]*height: 44[\s\S]*p: 0/);
  assert.match(sparkline, /repeat\(auto-fill, 44px\)/);
  assert.match(sparkline, /width: "100%"/);
  assert.match(sparkline, /repeat\(\$\{columns\}, \$\{desktopCell\}px\)/);
  assert.match(sparkline, /width: 44[\s\S]*height: 44/);
  assert.match(sparkline, /formatAccessibleDate\(run\.timestamp\)/);
  assert.match(dashboard, /useSearchParams\(\)/);
  assert.match(dashboard, /persistOverviewHistoryState\(\{ scrollY: window\.scrollY \}\)/);
  assert.match(dashboard, /<OverviewSectionNav \/>/);
  assert.match(sectionNav, /aria-label="Overview sections"/);
  assert.match(sectionNav, /minHeight: 44/);
  assert.match(sectionNav, /prefers-reduced-motion: reduce/);
  assert.match(sectionNav, /target\.focus\(\{ preventScroll: true \}\)/);
  assert.match(sectionNav, /target\.scrollIntoView/);
  assert.match(ledger, /\(min-width: 1024px\)/);
  assert.match(ledger, /JobHealthSection/);
  assert.match(ledger, /function MobileJobRow/);
  assert.match(ledger, /role="listitem"/);
  assert.match(ledger, /role="list"/);
  assert.match(ledger, /function DesktopJobRow/);
  assert.match(overviewTheme, /pageHeadline/);
  assert.match(overviewTheme, /mobileFeaturedBody/);
  assert.match(overviewTheme, /subsectionHeading/);
  assert.doesNotMatch(ledger, /position:\s*"sticky"/);
});

// Overview and Pulls share one tile filter, so the status word, its tone, and
// the optional count render the same way on both pages.
test("status filter tiles carry a label, a tone, and an optional count", () => {
  const options = [
    { value: "ALL", label: "All", tone: "var(--mui-palette-text-primary)", count: 29 },
    { value: "FAILING", label: "Failing", tone: "var(--mui-palette-error-main)", count: 6 },
  ];
  const counted = render(createElement(StatusFilterTiles, { label: "Presubmit state", value: "FAILING", options, onChange: () => {} }));
  assert.match(counted, /role="group"[^>]*aria-label="Presubmit state"/);
  assert.match(counted, /aria-pressed="true"[^>]*>(?:(?!<\/button>).)*>6<(?:(?!<\/button>).)*Failing/s);
  assert.match(counted, /--tone:var\(--mui-palette-error-main\)/);

  const labelsOnly = render(createElement(StatusFilterTiles, {
    label: "Job status filter",
    value: "ALL",
    options: options.map(({ value, label, tone }) => ({ value, label, tone })),
    onChange: () => {},
  }));
  assert.match(labelsOnly, />Failing</);
  assert.doesNotMatch(labelsOnly, />29<|>6</);
});

test("run change names where the current streak began and the run before it", () => {
  const run = (build_id: string, passed: boolean, day: number, result = passed ? "SUCCESS" : "FAILURE") =>
    ({ build_id, passed, result, timestamp: `2026-10-${String(day).padStart(2, "0")}T12:00:00Z` });
  // Newest first, as the dashboard publishes them.
  const broke = runChange([run("f3", false, 9), run("f2", false, 6), run("p1", true, 3)]);
  assert.deepEqual(broke && broke.kind === "broke" && [broke.changed.build_id, broke.before.build_id], ["f2", "p1"]);
  const recovered = runChange([run("p2", true, 9), run("f1", false, 6)]);
  assert.deepEqual(recovered && recovered.kind === "recovered" && [recovered.changed.build_id, recovered.before.build_id], ["p2", "f1"]);
  const noPass = runChange([run("f2", false, 6), run("f1", false, 3)]);
  assert.deepEqual(noPass && noPass.kind === "noPass" && noPass.since.build_id, "f1");
  // A run still in progress has no outcome, so the streak is read from the
  // newest finished run.
  const pending = runChange([run("r", false, 10, "PENDING"), run("f1", false, 6), run("p1", true, 3)]);
  assert.deepEqual(pending && pending.kind === "broke" && [pending.changed.build_id, pending.before.build_id], ["f1", "p1"]);
  assert.equal(runChange([run("r", false, 10, "PENDING")]), null);
  assert.equal(runChange([]), null);

  assert.equal(runDateLabel(run("f2", false, 6)), "Oct 6");
  assert.equal(runDateLabel({ build_id: "42", passed: false, timestamp: "invalid" }), "run 42");
});

test("featured pattern captions when the job broke with links to both runs", () => {
  const html = render(createElement(FeaturedPatternRow, {
    pattern: recurringPattern("main", 3, ["f2", "f1"], 0),
    rank: 1,
    prefix: "",
    job: job({
      job_id: "main",
      current_status: "FAILING",
      recent_runs: [
        { build_id: "f2", passed: false, result: "FAILURE", timestamp: "2026-10-09T12:00:00Z" },
        { build_id: "f1", passed: false, result: "FAILURE", timestamp: "2026-10-06T12:00:00Z" },
        { build_id: "p1", passed: true, result: "SUCCESS", timestamp: "2026-10-03T12:00:00Z" },
      ],
    }),
  }));
  const content = html.replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, "");
  assert.match(content, /Broke <a[^>]*href="\/job\/main\?run=f1"[^>]*>Oct 6<\/a> · last pass <a[^>]*href="\/job\/main\?run=p1"[^>]*>Oct 3<\/a>/);
  assert.match(html, /aria-label="First failing run, run f1, Oct 6, 2026"/);
});
