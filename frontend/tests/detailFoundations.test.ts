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
const { TestCaseTable } = (await vite.ssrLoadModule("/src/components/TestCaseTable.tsx")) as {
  TestCaseTable: (props: {
    testCases: TestCase[];
    jobID?: string;
    buildId?: string;
    buildLogUrl?: string;
    webUrl?: string;
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
    /aria-label="Open diagnosis for Fails cluster\. Failed\. Duration 5s"/,
  );
  const diagnosis = html.match(
    /<a[^>]*aria-label="Open diagnosis[^"]*"[^>]*>([\s\S]*?)<\/a>/,
  );
  assert.ok(diagnosis);
  assert.doesNotMatch(diagnosis[1], /<a|<button/);
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
