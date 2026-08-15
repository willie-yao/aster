import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { createServer } from "vite";

import { createSearchFuse, searchIndexPath } from "../src/lib/search.js";
import type { SearchEntry } from "../src/types/dashboard.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const searchBar = (await vite.ssrLoadModule(
  "/src/components/SearchBar.tsx",
)) as {
  SearchResultButton: ((props: {
    entry: SearchEntry;
    filePrefix: string;
    onSelect: (entry: SearchEntry) => void;
  }) => ReturnType<typeof createElement>) & {
    accessibleName: (entry: SearchEntry, filePrefix: string) => string;
    path: (entry: SearchEntry) => string;
  };
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
};
await vite.close();

const { SearchResultButton } = searchBar;
const searchResultAccessibleName = SearchResultButton.accessibleName;
const searchResultPath = SearchResultButton.path;

function testEntry(overrides: Partial<SearchEntry> = {}): SearchEntry {
  return {
    kind: "test",
    test_name: "Conformance Tests conformance-tests",
    job_name: "periodic-capz-e2e-main",
    job_id: "periodic-capz-e2e-main",
    job_type: "periodic",
    repo: "",
    tab_name: "",
    branch: "main",
    category: "Conformance",
    status: "failed",
    fail_rate: 0,
    ...overrides,
  };
}

test("search index stays disabled until search activation", () => {
  assert.equal(searchIndexPath(false), null);
  assert.equal(searchIndexPath(true), "search-index.json");
});

test("SearchBar finds exact terms later in indexed test names", () => {
  const entries = [
    testEntry({
      job_id: "vmss",
      test_name:
        "[It] Workload cluster creation Creating a VMSS cluster [REQUIRED] with a single control plane node and an AzureMachinePool with 2 nodes",
    }),
    testEntry({
      job_id: "azure-linux-3",
      test_name:
        "[It] Workload cluster creation Creating an Azure Linux 3 cluster with a managed machine pool",
    }),
    testEntry({
      job_id: "highly-available",
      test_name:
        "[It] Workload cluster creation Creating a highly-available cluster",
    }),
  ];
  const fuse = createSearchFuse(entries);

  for (const [query, jobID] of [
    ["vmss", "vmss"],
    ["Azure Linux 3", "azure-linux-3"],
    ["Highly-available", "highly-available"],
  ] as const) {
    assert.deepEqual(
      fuse.search(query).map((result) => result.item.job_id),
      [jobID],
      query,
    );
  }
});

test("SearchBar names its controls for jobs and tests", () => {
  const source = readFileSync(
    resolve(process.cwd(), "src/components/SearchBar.tsx"),
    "utf8",
  );

  assert.match(source, /aria-label="Search jobs and tests"/);
  assert.match(source, /placeholder="Search jobs and tests…"/);
  assert.match(
    source,
    /htmlInput: \{ "aria-label": "Search jobs and tests" \}/,
  );
});

test("SearchBar gives repeated conformance results unique job and branch context", () => {
  const entries = Array.from({ length: 5 }, (_, index) =>
    testEntry({
      job_name: `periodic-capz-conformance-${index + 1}`,
      job_id: `periodic-capz-conformance-${index + 1}`,
      branch: index < 3 ? "main" : `release-1-${index + 28}`,
    }),
  );

  const names = entries.map((entry) =>
    searchResultAccessibleName(entry, "periodic-capz-"),
  );
  assert.equal(new Set(names).size, 5);
  assert.ok(
    names.every((name) =>
      name.startsWith("Conformance: conformance-tests, job periodic-capz-conformance-"),
    ),
  );
  assert.ok(names.every((name) => name.includes(", branch ")));
});

test("SearchBar disambiguates same architecture test across jobs and repositories", () => {
  const entries = [
    ...Array.from({ length: 6 }, (_, index) =>
      testEntry({
        test_name: "architecture-test",
        job_name: `periodic-capz-architecture-${index + 1}`,
        job_id: `periodic-capz-architecture-${index + 1}`,
        branch: index < 3 ? "main" : `release-1-${index + 28}`,
      }),
    ),
    testEntry({
      test_name: "architecture-test",
      job_name: "pull-capz-architecture",
      job_id:
        "kubernetes-sigs/cluster-api-provider-azure/pull-capz-architecture",
      job_type: "presubmit",
      repo: "kubernetes-sigs/cluster-api-provider-azure",
      branch: "main",
    }),
    testEntry({
      test_name: "architecture-test",
      job_name: "pull-capz-architecture",
      job_id: "example/cluster-api-provider-azure/pull-capz-architecture",
      job_type: "presubmit",
      repo: "example/cluster-api-provider-azure",
      branch: "main",
    }),
  ];

  const names = entries.map((entry) =>
    searchResultAccessibleName(entry, "periodic-capz-"),
  );
  assert.equal(new Set(names).size, 8);
  assert.ok(names.every((name) => name.startsWith("Architecture-test, job ")));
  assert.match(
    names[6],
    /repository kubernetes-sigs\/cluster-api-provider-azure, branch main$/,
  );
  assert.match(
    names[7],
    /repository example\/cluster-api-provider-azure, branch main$/,
  );
});

test("SearchBar result button exposes the descriptive accessible name", () => {
  const entry = testEntry({ fail_rate: 0.25 });
  const name = searchResultAccessibleName(entry, "periodic-capz-");
  const html = renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: defaultTheme },
      createElement(SearchResultButton, {
        entry,
        filePrefix: "periodic-capz-",
        onSelect: () => undefined,
      }),
    ),
  );

  assert.equal(
    name,
    "Conformance: conformance-tests, job periodic-capz-e2e-main, branch main, 25% failure rate",
  );
  assert.match(
    html,
    /<button[^>]*aria-label="Conformance: conformance-tests, job periodic-capz-e2e-main, branch main, 25% failure rate"/,
  );
});

test("SearchBar keeps job and test navigation targets", () => {
  const job = testEntry({
    kind: "job",
    test_name: "",
    job_name: "pull-capz-e2e",
    job_id: "kubernetes-sigs/cluster-api-provider-azure/pull-capz-e2e",
    job_type: "presubmit",
    repo: "kubernetes-sigs/cluster-api-provider-azure",
    tab_name: "CAPZ pull request",
  });
  const result = testEntry({
    job_id: "kubernetes-sigs/cluster-api-provider-azure/pull-capz-e2e",
    test_name: "[It] validates A/B?",
  });

  assert.equal(
    searchResultPath(job),
    "/job/kubernetes-sigs%2Fcluster-api-provider-azure%2Fpull-capz-e2e",
  );
  assert.equal(
    searchResultPath(result),
    "/job/kubernetes-sigs%2Fcluster-api-provider-azure%2Fpull-capz-e2e/test/%5BIt%5D%20validates%20A%2FB%3F",
  );
});
