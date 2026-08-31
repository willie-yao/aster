import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import {
  attentionSignal,
  countLabel,
  currentPatternFailureStreak,
  disclosureLabel,
  mergeOverviewHistoryState,
  needsAttentionSummary,
  orderedDashboardBranches,
  patternEvidenceLabel,
  rankRecurringPatterns,
  overviewBranchFromParam,
  overviewStatusFromParam,
  readOverviewHistoryState,
  withOverviewFilters,
} from "../src/lib/dashboardOverview.js";
import type { BuildResult, JobSummary, PatternAnalysis } from "../src/types/dashboard.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
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
    stale: boolean;
    job?: JobSummary;
  }) => ReturnType<typeof createElement>;
};
const { RunHistory } = (await vite.ssrLoadModule("/src/components/RunHistory.tsx")) as {
  RunHistory: (props: {
    runs: BuildResult[];
    selectedBuildId?: string;
    onSelect: (buildId: string) => void;
  }) => ReturnType<typeof createElement>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
};
await vite.close();

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

test("health summary exposes counts and pressed filter state", () => {
  const jobs = [
    job(),
    job({ job_id: "flaky", name: "flaky", overall_status: "FLAKY" }),
    job({ job_id: "failing", name: "failing", overall_status: "FAILING" }),
  ];
  const html = render(createElement(HealthPanel, { jobs, activeFilter: "FLAKY", onFilterClick: () => undefined }));

  assert.match(html, /Reliability over the last 10 runs/);
  assert.match(html, /3 jobs/);
  assert.match(html, /aria-label="Passing: 1 job, 33% of jobs\./);
  assert.match(html, /aria-label="Flaky: 1 job, 33% of jobs\./);
  assert.match(html, /mix of passing and failing results/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /aria-label="Failing: 1 job, 33% of jobs\./);
  assert.match(html, />33% of jobs</);
  assert.match(html, /inset 0 -3px 0/);
});

test("job health ledger keeps job and run links separate", () => {
  const html = render(createElement(JobHealthTable, { sections: [{ id: "capz-e2e", label: "CAPZ E2E", jobs: [job()] }] }));

  assert.match(html, /role="table"/);
  assert.match(html, /role="cell"[^>]*>[\s\S]*<h3[^>]*>CAPZ E2E<\/h3>/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main"/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main\?run=123"/);
  assert.match(html, /aria-label="Run 123, passed, Aug 5, 2026"/);
  assert.match(html, />Last 10 pass</);
  assert.match(html, />Current</);
  assert.match(html, />Passing</);
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

  const maxRuns = number(/const maxRuns = (\d+)/, sparkline);
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
  assert.equal(attentionSignal("high", false), "high confidence");
  assert.equal(attentionSignal("medium", true), "medium confidence · Last successful refresh");
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

test("recurring pattern evidence distinguishes matching failures from analyzed builds", () => {
  const partial = recurringPattern("partial", 5, ["2", "1"], 0);
  const active = recurringPattern("active", 3, ["3", "2", "1"], 0);

  assert.equal(patternEvidenceLabel(partial), "2 same-cause failures across 5 analyzed builds");
  assert.equal(patternEvidenceLabel(active), "3 same-cause failures");
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
    stale: false,
    job: activeJob,
  }));

  assert.match(html, /Failing now · 3 in a row/);
  assert.match(html, /3 same-cause failures/);
  assert.doesNotMatch(html, />Flaky</);

  const running = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    stale: false,
    job: {
      ...activeJob,
      current_status: "RUNNING",
      recent_runs: [
        { build_id: "pending", passed: false, result: "PENDING", timestamp: "2026-08-28T18:00:00Z" },
        ...activeJob.recent_runs,
      ],
    },
  }));
  assert.match(running, /Running now · after 3 same-cause failures/);
  assert.doesNotMatch(running, /Failing now/);
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
    stale: false,
    job: failingJob,
  }));
  assert.match(failing, /Failing now/);
  assert.match(failing, /3 same-cause failures across 4 analyzed builds/);
  assert.doesNotMatch(failing, /in a row/);
  assert.doesNotMatch(failing, />Flaky</);

  const passing = render(createElement(FeaturedPatternRow, {
    pattern,
    rank: 1,
    prefix: "",
    stale: false,
    job: { ...failingJob, current_status: "PASSING" },
  }));
  assert.match(passing, /Passing now/);
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
    stale: false,
    job: job({ overall_status: "FAILING" }),
  }));
  const destinations = [...html.matchAll(/<a\b[^>]*href="([^"]+)"/g)].map((match) => match[1]);

  assert.equal(destinations[0], "/job/capz-periodic-e2e-main");
  assert.deepEqual(destinations.slice(1), [
    "/job/capz-periodic-e2e-main?run=122",
    "/job/capz-periodic-e2e-main?run=123",
  ]);
  assert.match(html, /aria-label="View analysis for capz-periodic-e2e-main"/);
  assert.match(html, />View analysis →</);
  assert.doesNotMatch(html, /<a\b[^>]*>(?:(?!<\/a>)[\s\S])*<a\b/);
  assert.doesNotMatch(html, /tabindex="[1-9]/);
});

test("overview filters use canonical query parameters", () => {
  assert.equal(overviewStatusFromParam("flaky"), "FLAKY");
  assert.equal(overviewStatusFromParam("unknown"), "ALL");
  assert.equal(overviewBranchFromParam("main", ["main", "release-1.26"]), "main");
  assert.equal(overviewBranchFromParam("release-1.25", ["main"]), "ALL");

  const filtered = withOverviewFilters(
    new URLSearchParams("trace=1&status=passing&branch=release-1.24"),
    "FLAKY",
    "main",
  );
  assert.equal(filtered.toString(), "trace=1&status=flaky&branch=main");
  assert.equal(withOverviewFilters(filtered, "ALL", "ALL").toString(), "trace=1");
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
  assert.match(filters, /aria-label="Reliability over the last 10 runs"/);
  assert.match(filters, /height: 44/);
  assert.match(filters, /boxShadow: "inset 0 -3px 0/);
  assert.match(filters, /color: "text.primary"/);
  assert.match(filters, /borderRadius: "0 !important"/);
  assert.match(filters, /borderRadius: "4px 0 0 4px !important"/);
  assert.match(filters, /borderRadius: "0 4px 4px 0 !important"/);
  assert.match(health, /onFilterClick\?\.\(active \? "ALL" : row\.status\)/);
  assert.match(health, /borderLeft: "1px solid"/);
  assert.match(health, /% of jobs/);
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
