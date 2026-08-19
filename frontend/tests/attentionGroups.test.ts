import assert from "node:assert/strict";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import {
  attentionGroupNoun,
  attentionGroups,
  lowPassRateLabel,
  MAX_ATTENTION_ITEMS,
  passRateSummary,
} from "../src/lib/dashboardOverview.js";
import type {
  FlakinessReport,
  JobSummary,
  LowPassRateEntry,
  TestFlakiness,
} from "../src/types/dashboard.js";
import type { Manifest } from "../src/types/manifest.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { NeedsAttention } = (await vite.ssrLoadModule("/src/components/NeedsAttention.tsx")) as {
  NeedsAttention: (props: {
    report: FlakinessReport | null;
    loading: boolean;
    error: string | null;
    jobsByID: Record<string, JobSummary>;
  }) => ReturnType<typeof createElement>;
};
const { ManifestContext } = (await vite.ssrLoadModule("/src/hooks/useManifest.ts")) as {
  ManifestContext: React.Context<Manifest | null>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
};
await vite.close();

function manifest(threshold?: number): Manifest {
  return {
    id: "example",
    name: "Example",
    source: {},
    testgrid: { dashboard: "sig-foo" },
    storage: { provider: "gcs", bucket: "logs" },
    branding: {
      title: "Example",
      base_path: "/example",
      site_url: "https://example.github.io/example",
      source_repo: { owner: "example", name: "example" },
    },
    attention: threshold === undefined ? undefined : { low_pass_rate: { threshold } },
  } as Manifest;
}

function renderAttention(report: FlakinessReport, threshold?: number): string {
  return renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: defaultTheme },
      createElement(
        MemoryRouter,
        null,
        createElement(
          ManifestContext.Provider,
          { value: manifest(threshold) },
          createElement(NeedsAttention, {
            report,
            loading: false,
            error: null,
            jobsByID: {},
          }),
        ),
      ),
    ),
  );
}

function flakyTest(testName: string, overrides: Partial<TestFlakiness> = {}): TestFlakiness {
  return {
    test_name: testName,
    job_name: "periodic-example",
    job_id: "example/periodic-example",
    total_runs: 10,
    failures: 1,
    passes: 9,
    flip_rate: 0.2,
    fail_rate: 0.1,
    consecutive_failures: 0,
    classification: "one-off",
    ...overrides,
  };
}

function lowPassRateEntry(testName: string, passRate: number): LowPassRateEntry {
  return { ...flakyTest(testName), window_runs: 10, pass_rate: passRate };
}

function report(overrides: Partial<FlakinessReport> = {}): FlakinessReport {
  return {
    generated_at: "2026-03-15T12:00:00Z",
    most_flaky: [],
    persistent_failures: [],
    recently_broken: [],
    build_failures: [],
    ...overrides,
  };
}

function labels(groups: ReturnType<typeof attentionGroups>): string[] {
  return groups.map((group) => group.label);
}

test("attention groups keep classification sections leading and flaky as fallback", () => {
  assert.deepEqual(labels(attentionGroups(null)), []);
  assert.deepEqual(labels(attentionGroups(report())), []);

  const primary = attentionGroups(report({
    recently_broken: [flakyTest("TestBroken")],
    persistent_failures: [flakyTest("TestPersistent")],
    most_flaky: [flakyTest("TestFlaky")],
  }));
  assert.deepEqual(labels(primary), ["Recent failures", "Persistent failures"]);

  const fallback = attentionGroups(report({ most_flaky: [flakyTest("TestFlaky")] }));
  assert.deepEqual(labels(fallback), ["Flaky tests"]);
});

test("pass-rate group is absent unless the report carries the section", () => {
  assert.deepEqual(labels(attentionGroups(report({ low_pass_rate: [] }), 1)), []);

  const groups = attentionGroups(report({ low_pass_rate: [lowPassRateEntry("TestOnce", 0.9)] }), 1);
  assert.deepEqual(labels(groups), ["Below 100% pass rate"]);
  assert.deepEqual(groups[0].items.map((item) => item.test_name), ["TestOnce"]);
});

test("pass-rate group label reports the configured cutoff", () => {
  assert.equal(lowPassRateLabel(1), "Below 100% pass rate");
  assert.equal(lowPassRateLabel(0.955), "Below 95.5% pass rate");
  assert.equal(lowPassRateLabel(0), "Below 0% pass rate");
  assert.equal(lowPassRateLabel(undefined), "Low pass rate");
  assert.equal(lowPassRateLabel(Number.NaN), "Low pass rate");
});

test("pass-rate group does not repeat a test an earlier group already listed", () => {
  const shared = flakyTest("TestShared");
  const groups = attentionGroups(report({
    recently_broken: [shared],
    low_pass_rate: [
      { ...shared, window_runs: 10, pass_rate: 0.9 },
      lowPassRateEntry("TestOther", 0.8),
    ],
  }), 1);

  assert.deepEqual(labels(groups), ["Recent failures", "Below 100% pass rate"]);
  assert.deepEqual(groups[1].items.map((item) => item.test_name), ["TestOther"]);
});

test("pass-rate group is dropped when the leading groups exhaust the item budget", () => {
  const broken = Array.from({ length: MAX_ATTENTION_ITEMS }, (_, i) => flakyTest(`TestBroken${i}`));
  const groups = attentionGroups(report({
    recently_broken: broken,
    low_pass_rate: [lowPassRateEntry("TestLow", 0.5)],
  }), 1);

  assert.deepEqual(labels(groups), ["Recent failures"]);
  assert.equal(groups[0].items.length, MAX_ATTENTION_ITEMS);
});

test("attention groups share one item budget across every group", () => {
  const groups = attentionGroups(report({
    recently_broken: Array.from({ length: 6 }, (_, i) => flakyTest(`TestBroken${i}`)),
    persistent_failures: Array.from({ length: 6 }, (_, i) => flakyTest(`TestPersistent${i}`)),
    low_pass_rate: Array.from({ length: 6 }, (_, i) => lowPassRateEntry(`TestLow${i}`, 0.5)),
  }), 1);

  const total = groups.reduce((sum, group) => sum + group.items.length, 0);
  assert.equal(total, MAX_ATTENTION_ITEMS);
  assert.deepEqual(labels(groups), ["Recent failures", "Persistent failures"]);
});

test("pass-rate summary reports the measured window, not the whole-window rate", () => {
  // recent_runs narrowed this entry to 6 runs while fail_rate covers 10.
  const entry: LowPassRateEntry = {
    ...flakyTest("TestNarrow", { total_runs: 10, fail_rate: 0.1 }),
    window_runs: 6,
    pass_rate: 5 / 6,
  };
  assert.equal(passRateSummary(entry), "83.3% pass rate over 6 runs");
  assert.equal(
    passRateSummary({ ...entry, window_runs: 1, pass_rate: 0 }),
    "0% pass rate over 1 run",
  );
});

test("pass-rate group discloses its own noun", () => {
  assert.deepEqual(attentionGroupNoun("lowPassRate"), [
    "additional low pass rate test",
    "additional low pass rate tests",
  ]);
  assert.deepEqual(attentionGroupNoun("persistent"), [
    "additional persistent failure",
    "additional persistent failures",
  ]);
  assert.deepEqual(attentionGroupNoun("flaky"), [
    "additional flaky test",
    "additional flaky tests",
  ]);
});

test("a recovered pass-rate selection is not styled or labeled as failing", () => {
  const recovered: LowPassRateEntry = {
    ...flakyTest("TestRecovered", { consecutive_failures: 0, classification: "one-off" }),
    window_runs: 10,
    pass_rate: 0.9,
  };
  const markup = renderAttention(report({ low_pass_rate: [recovered] }), 1);

  assert.match(markup, /Below 100% pass rate/);
  assert.match(markup, /90% pass rate over 10 runs/);
  assert.match(markup, /One-off/);
  assert.doesNotMatch(markup, /Failing/);
});

test("a currently failing test still reads as failing outside the pass-rate group", () => {
  const broken = flakyTest("TestBroken", {
    consecutive_failures: 2,
    classification: "persistent",
    first_failed_at: "2026-03-15T10:00:00Z",
  });
  const markup = renderAttention(report({ recently_broken: [broken] }));

  assert.match(markup, /Recent failures/);
  assert.match(markup, /Failing/);
  assert.match(markup, /2 consecutive failures/);
});
