import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { createServer } from "vite";
import { MemoryRouter } from "react-router-dom";
import { parseTestDisplayName } from "../src/lib/detailTitles.js";
import type { BuildResult, TestCase } from "../src/types/dashboard.js";
import type { FetchProgressStatus, FetchStatusResponse } from "../src/types/fetchStatus.js";
import type { AIUsageDaily } from "../src/types/usage.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { DetailSectionBand } = (await vite.ssrLoadModule("/src/components/DetailSectionBand.tsx")) as {
  DetailSectionBand: (props: {
    title: string;
    metadata?: string;
    headingLevel?: "h2" | "h3";
  }) => ReturnType<typeof createElement>;
};
const { RunHistory } = (await vite.ssrLoadModule("/src/components/RunHistory.tsx")) as {
  RunHistory: (props: {
    runs: BuildResult[];
    selectedBuildId?: string;
    onSelect: (buildId: string) => void;
    metadata?: string;
  }) => ReturnType<typeof createElement>;
};
const { MetricStrip } = (await vite.ssrLoadModule("/src/components/MetricStrip.tsx")) as {
  MetricStrip: (props: {
    label: string;
    items: Array<{ label: string; value: string; note?: string }>;
  }) => ReturnType<typeof createElement>;
};
const { DaySummaryButton, HistoricalTable } = (await vite.ssrLoadModule("/src/components/AIUsageDaily.tsx")) as {
  DaySummaryButton: (props: {
    day: AIUsageDaily;
    open: boolean;
    controls: string;
    onToggle: () => void;
  }) => ReturnType<typeof createElement>;
  HistoricalTable: (props: { days: AIUsageDaily[] }) => ReturnType<typeof createElement>;
};
const { JobDetailPrimaryLayout } = (await vite.ssrLoadModule("/src/pages/JobDetailPage.tsx")) as {
  JobDetailPrimaryLayout: (props: {
    patternAnalysis?: ReturnType<typeof createElement>;
    buildFailureAnalysis?: ReturnType<typeof createElement>;
    runHistory: ReturnType<typeof createElement>;
    runMetadata: ReturnType<typeof createElement>;
  }) => ReturnType<typeof createElement>;
};
const { BuildFailurePanel } = (await vite.ssrLoadModule("/src/components/BuildFailurePanel.tsx")) as {
  BuildFailurePanel: (props: {
    jobID: string;
    run: BuildResult;
    failure: TestCase;
    fetchStatus: FetchStatusResponse | null;
    showDetailLink?: boolean;
    briefingTitle?: string;
    mobileBriefingTitle?: string;
    beforeActions?: ReturnType<typeof createElement>;
  }) => ReturnType<typeof createElement>;
};
const { TestCaseTable, EvidenceSourceLink } = (await vite.ssrLoadModule("/src/components/TestCaseTable.tsx")) as {
  TestCaseTable: (props: {
    testCases: TestCase[];
    jobID?: string;
    buildId?: string;
    buildLogUrl?: string;
    webUrl?: string;
  }) => ReturnType<typeof createElement>;
  EvidenceSourceLink: (props: {
    href: string;
    label: string;
    text: string;
  }) => ReturnType<typeof createElement>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
};
await vite.close();

function render(element: ReturnType<typeof createElement>): string {
  return renderToStaticMarkup(
    createElement(ThemeProvider, { theme: defaultTheme }, element),
  );
}

function run(overrides: Partial<BuildResult> = {}): BuildResult {
  return {
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
    ...overrides,
  };
}

function usageDay(overrides: Partial<AIUsageDaily> = {}): AIUsageDaily {
  return {
    date: "2026-08-10",
    totals: {
      operations: 10094,
      cache_hits: 9841,
      failures: 2,
      external_unmetered_operations: 0,
      model_requests: 1417,
      reported_requests: 1400,
      unreported_requests: 17,
      input_tokens: 100000,
      cached_input_tokens: 70000,
      cache_write_input_tokens: 5000,
      output_tokens: 20000,
      reasoning_tokens: 3000,
      estimated_cost_nanos: "0",
    },
    features: [],
    coverage: {
      status: "partial",
      states: ["partial_token_usage"],
      model_requests: 1417,
      reported_requests: 1400,
      unreported_requests: 17,
      external_unmetered_operations: 0,
    },
    has_usage: true,
    current_partial_utc: true,
    recorded_cost_status: "unknown",
    current_rate_status: "available",
    current_rate_currency: "USD",
    current_rate_estimated_cost_nanos: "23470000000",
    ...overrides,
  };
}

const buildFetchProgress: FetchProgressStatus = {
  schema_version: 6,
  run_id: "run",
  pass_id: "pass",
  pass_type: "initial-watch",
  phase: "analysis",
  run_started_at: "2026-08-11T00:00:00Z",
  pass_started_at: "2026-08-11T00:00:00Z",
  phase_started_at: "2026-08-11T00:00:00Z",
  last_progress_at: "2026-08-11T00:00:00Z",
  outcome: "running",
  jobs: { total: 1, completed: 1 },
  builds: { cached: 0, fetched: 1 },
  analyses: {
    logical_total: 1,
    accepted_cache_hits: 0,
    compatible_results_reused: 0,
    new_work: 1,
    stale_work: 0,
    queued: 1,
    running: 0,
    completed: 0,
    failed: 0,
    cancelled: 0,
    task_attempts: 0,
    retries: 0,
    existing_tasks_adopted: 0,
    results_retrieved: 0,
    result_retrieval_retries: 0,
    build_subjects: {
      logical_total: 1,
      queued: 1,
      running: 0,
      completed: 0,
      failed: 0,
      cancelled: 0,
      accepted_cache_hits: 0,
      existing_tasks_adopted: 0,
    },
  },
  pattern_phase: "pending",
  publication_phase: "pending",
  side_effect_phase: "pending",
};

const titleCases = [
  {
    name: "label-heavy",
    input: "[It] [sig-node] [DRA] kubelet [Feature:DynamicResourceAllocation] [FeatureGate:DRADeviceTaints] [FeatureGate:DRAWorkloadResourceClaims] [FeatureGate:GenericWorkload] [KubeletMinVersion:1.36] DeviceTaintRule evicts pod with PodGroup claim [FeatureGate:DRADeviceTaintRules] [Beta] [Feature:OffByDefault] [FeatureGate:DynamicResourceAllocation]",
    displayName: "DeviceTaintRule evicts pod with PodGroup claim",
    labels: [
      "[It]",
      "[sig-node]",
      "[DRA]",
      "[Feature:DynamicResourceAllocation]",
      "[FeatureGate:DRADeviceTaints]",
      "[FeatureGate:DRAWorkloadResourceClaims]",
      "[FeatureGate:GenericWorkload]",
      "[KubeletMinVersion:1.36]",
      "[FeatureGate:DRADeviceTaintRules]",
      "[Beta]",
      "[Feature:OffByDefault]",
      "[FeatureGate:DynamicResourceAllocation]",
    ],
    prefixes: ["kubelet"],
    fallback: false,
  },
  {
    name: "simple",
    input: "[It] creates a workload cluster",
    displayName: "Creates a workload cluster",
    labels: ["[It]"],
    prefixes: [],
    fallback: false,
  },
  {
    name: "unlabeled",
    input: "reconciles AzureMachinePool instances",
    displayName: "Reconciles AzureMachinePool instances",
    labels: [],
    prefixes: [],
    fallback: false,
  },
  {
    name: "unstructured kubelet prefix",
    input: "kubelet reports device status",
    displayName: "Kubelet reports device status",
    labels: [],
    prefixes: [],
    fallback: false,
  },
  {
    name: "literal brackets",
    input: "[It] validates literal [brackets preserved] in output",
    displayName: "Validates literal [brackets preserved] in output",
    labels: ["[It]"],
    prefixes: [],
    fallback: false,
  },
  {
    name: "suite-prefixed",
    input: "[It] Workload cluster creation Creating a highly-available cluster",
    displayName: "Highly-available cluster",
    labels: ["[It]"],
    prefixes: ["Workload cluster creation Creating a"],
    fallback: false,
  },
  {
    name: "structured suite prefix capture",
    input: "[It] Running the Cluster API E2E tests should create a cluster",
    displayName: "Should create a cluster",
    labels: ["[It]"],
    prefixes: ["Running the Cluster API E2E tests"],
    fallback: false,
  },
  {
    name: "legitimate running title",
    input: "[It] Running pods retain readiness",
    displayName: "Running pods retain readiness",
    labels: ["[It]"],
    prefixes: [],
    fallback: false,
  },
  {
    name: "unicode",
    input: "[It] [sig-node] validates café 节点 readiness",
    displayName: "Validates café 节点 readiness",
    labels: ["[It]", "[sig-node]"],
    prefixes: [],
    fallback: false,
  },
  {
    name: "empty result",
    input: "[It] [sig-node] [DRA]",
    displayName: "[It] [sig-node] [DRA]",
    labels: ["[It]", "[sig-node]", "[DRA]"],
    prefixes: [],
    fallback: true,
  },
] as const;

for (const tc of titleCases) {
  test(`test display title: ${tc.name}`, () => {
    const result = parseTestDisplayName(tc.input);
    assert.equal(result.displayName, tc.displayName);
    assert.deepEqual(result.labels, [...tc.labels]);
    assert.deepEqual(result.removedPrefixes, [...tc.prefixes]);
    assert.equal(result.usedFallback, tc.fallback);
  });
}

test("detail section band keeps metadata separate from its heading", () => {
  const html = render(createElement(DetailSectionBand, {
    title: "Analysis briefing",
    metadata: "9 failures · high confidence",
  }));

  assert.match(html, /<h2[^>]*>Analysis briefing<\/h2>/);
  assert.match(html, />9 failures · high confidence</);
});

test("run history exposes square selected runs with date and result context", () => {
  const html = render(createElement(RunHistory, {
    runs: [
      run(),
      run({ build_id: "124", passed: true, result: "SUCCESS", started: "2026-08-06T10:00:00Z" }),
    ],
    selectedBuildId: "123",
    onSelect: () => undefined,
    metadata: "1 failed · 1 passed",
  }));

  assert.match(html, /<h2[^>]*>Run history<\/h2>/);
  assert.doesNotMatch(html, /<h3[^>]*>Run history<\/h3>/);
  assert.match(html, /aria-label="#123 · Failed · Aug 5, 2026"/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /aria-label="#124 · Passed · Aug 6, 2026"/);
  assert.match(html, />Selected #123 · Failed</);
});

test("mobile usage day disclosure names the accounting summary", () => {
  const html = render(createElement(DaySummaryButton, {
    day: usageDay(),
    open: false,
    controls: "usage-mobile-day-2026-08-10",
    onToggle: () => undefined,
  }));

  assert.match(
    html,
    /aria-label="Expand feature breakdown for 2026-08-10\. 10,094 operations\. 1,417 requests\. 9,841 cache hits\. Recorded estimate Unknown\. Current-rate reprice USD 23\.47\. Partial coverage\. Partial UTC day\."/,
  );
});

test("collapsed desktop usage days do not add empty table rows", () => {
  const html = render(createElement(HistoricalTable, { days: [usageDay()] }));
  assert.equal(html.match(/<tr(?:\s|>)/g)?.length, 2);
});

test("metric strip retains qualification notes without changing its shared geometry", () => {
  const html = render(createElement(MetricStrip, {
    label: "Usage metrics",
    items: [
      { label: "Recorded estimate", value: "USD 1.25", note: "Stored per-operation prices" },
      { label: "Requests", value: "7" },
    ],
  }));

  assert.match(html, /aria-label="Usage metrics"/);
  assert.match(html, />Recorded estimate</);
  assert.match(html, />USD 1\.25</);
  assert.match(html, />Stored per-operation prices</);
  assert.match(html, />Requests</);
});

test("job detail primary layout keeps analysis before the run rail in every state", () => {
  const cases = [
    { name: "pattern and build", pattern: true, build: true, order: ["Pattern analysis", "Build analysis", "Run history", "Run metadata"] },
    { name: "pattern only", pattern: true, build: false, order: ["Pattern analysis", "Run history", "Run metadata"] },
    { name: "build only", pattern: false, build: true, order: ["Build analysis", "Run history", "Run metadata"] },
    { name: "neither", pattern: false, build: false, order: ["Run history", "Run metadata"] },
  ];

  for (const tc of cases) {
    const html = render(createElement(JobDetailPrimaryLayout, {
      patternAnalysis: tc.pattern ? createElement("section", null, "Pattern analysis") : undefined,
      buildFailureAnalysis: tc.build ? createElement("section", null, "Build analysis") : undefined,
      runHistory: createElement("section", null, "Run history"),
      runMetadata: createElement("section", null, "Run metadata"),
    }));

    let previous = -1;
    for (const label of tc.order) {
      const current = html.indexOf(label);
      assert.ok(current > previous, `${tc.name}: ${label}`);
      assert.equal(html.indexOf(label, current + 1), -1, `${tc.name}: duplicate ${label}`);
      previous = current;
    }
    if (!tc.pattern) assert.doesNotMatch(html, /Pattern analysis/, tc.name);
    if (!tc.build) assert.doesNotMatch(html, /Build analysis/, tc.name);
  }
});

test("standalone non-success build states avoid empty mobile diagnosis and action surfaces", () => {
  const failure: TestCase = {
    name: "Prow job execution",
    source: "build",
    status: "failed",
    duration_seconds: 1,
    ai_summary: {
      generated_at: "2026-08-11T00:00:00Z",
      summary: "Published build summary",
      is_transient: false,
    },
  };
  const states: Array<{
    name: string;
    fetchStatus: FetchStatusResponse | null;
    notice: string;
  }> = [
    {
      name: "pending",
      fetchStatus: { available: true, state: "active", status: buildFetchProgress },
      notice: "Build analysis pending",
    },
    {
      name: "stale",
      fetchStatus: { available: true, state: "stale", status: buildFetchProgress },
      notice: "Build analysis status stale",
    },
    { name: "unavailable", fetchStatus: null, notice: "Build analysis unavailable" },
  ];

  for (const state of states) {
    const html = render(
      createElement(
        MemoryRouter,
        { initialEntries: ["/"] },
        createElement(BuildFailurePanel, {
          jobID: "capz-periodic-e2e-main",
          run: run(),
          failure,
          fetchStatus: state.fetchStatus,
          showDetailLink: false,
          briefingTitle: "Analysis briefing",
          mobileBriefingTitle: "Analysis briefing",
          beforeActions: createElement("div", null, "Run metadata sentinel"),
        }),
      ),
    );

    assert.match(html, new RegExp(state.notice), state.name);
    assert.match(html, />Published build summary</, state.name);
    assert.match(html, />Run metadata sentinel</, state.name);
    assert.doesNotMatch(html, /Full diagnosis/, state.name);
    assert.doesNotMatch(html, /<hr/, state.name);
    assert.doesNotMatch(html, /aria-label="Build failure actions"/, state.name);
  }
});

test("test result navigation names include status duration and source context", () => {
  const testCase: TestCase = {
    name: "[It] fails cluster",
    status: "failed",
    duration_seconds: 5,
    failure_message: "boom",
    failure_location_url: "https://github.com/example/repo/blob/main/test.go#L10",
  };
  const html = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/"] },
      createElement(TestCaseTable, {
        testCases: [testCase],
        jobID: "job-main",
        buildId: "123",
      }),
    ),
  );

  assert.match(
    html,
    /aria-label="Open analysis for Fails cluster\. Failed\. Duration 5s"/,
  );
  const analysis = html.match(
    /<a[^>]*aria-label="Open analysis[^"]*"[^>]*>([\s\S]*?)<\/a>/,
  );
  assert.ok(analysis);
  assert.doesNotMatch(analysis[1], /<a|<button/);
});

test("expanded evidence source links keep context and a 44px target", () => {
  const html = render(createElement(EvidenceSourceLink, {
    href: "https://github.com/example/repo/blob/main/test.go#L10",
    label: "View source for Fails cluster on GitHub",
    text: "test.go:10",
  }));

  assert.match(html, /aria-label="View source for Fails cluster on GitHub"/);
  assert.match(html, /min-height:44px/);
  assert.match(html, /:focus-visible/);
});

test("shared detail foundations follow the Overview structural language", () => {
  const band = readFileSync(resolve(process.cwd(), "src/components/DetailSectionBand.tsx"), "utf8");
  const history = readFileSync(resolve(process.cwd(), "src/components/RunHistory.tsx"), "utf8");

  assert.match(band, /boxShadow: "inset 3px 0 0 var\(--mui-palette-primary-main\)"/);
  assert.match(band, /gridTemplateAreas: \{ xs: '"title" "metadata"'/);
  assert.match(history, /width: \{ xs: 44, sm: 32 \}/);
  assert.match(history, /height: \{ xs: 44, sm: 32 \}/);
  assert.match(history, /borderRadius: "2px"/);
  assert.match(history, /outlineColor: isSelected \? "primary\.main" : "transparent"/);
  assert.match(history, /boxShadow: "0 0 0 5px var\(--mui-palette-background-default\), 0 0 0 7px var\(--mui-palette-text-primary\)"/);
  assert.match(history, /Scroll runs ↔/);
});
