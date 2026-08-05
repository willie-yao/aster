import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import { orderedDashboardBranches } from "../src/lib/dashboardOverview.js";
import type { JobSummary } from "../src/types/dashboard.js";

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
  JobHealthTable: (props: { jobs: JobSummary[] }) => ReturnType<typeof createElement>;
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

function render(element: ReturnType<typeof createElement>): string {
  return renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: defaultTheme },
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

  assert.match(html, /Health/);
  assert.match(html, /3 jobs/);
  assert.match(html, /aria-label="Passing: 1 jobs, 33%"/);
  assert.match(html, /aria-label="Flaky: 1 jobs, 33%"/);
  assert.match(html, /aria-pressed="true"/);
  assert.match(html, /aria-label="Failing: 1 jobs, 33%"/);
});

test("job health ledger keeps job and run links separate", () => {
  const html = render(createElement(JobHealthTable, { jobs: [job()] }));

  assert.match(html, /role="table"/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main"/);
  assert.match(html, /href="\/job\/capz-periodic-e2e-main\?run=123"/);
  assert.match(html, />Passing</);
  assert.doesNotMatch(html, /<a\b[^>]*>(?:(?!<\/a>)[\s\S])*<a\b/);
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

  assert.match(dashboard, /<JobHealthTable jobs=/);
  assert.doesNotMatch(dashboard, /JobCard/);
  assert.doesNotMatch(dashboard, /HealthDonut/);
  assert.doesNotMatch(attention, /overflowY:\s*"auto"/);
  assert.match(attention, /jobPath\(pattern\.job_id/);
  assert.match(attention, /testRunPath\(item\.job_id, item\.test_name, item\.last_failure\.build_id\)/);
});
