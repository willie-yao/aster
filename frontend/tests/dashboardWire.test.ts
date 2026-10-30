import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ComponentType, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import { checkState, checkStatusLabel, checkSummaryLine } from "../src/lib/pullRequests.js";
import { evidenceMember, sharedFailureAnalyzable } from "../src/lib/sharedFailures.js";
import { jobPath } from "../src/lib/routes.js";
import type { Dashboard, JobDetail, JobSummary } from "../src/types/dashboard.js";
import type { Manifest } from "../src/types/manifest.js";
import type {
  PullRequestCheck,
  PullRequestDetail,
  SharedFailureIndex,
  SharedFailureMember,
} from "../src/types/pullRequests.js";

const wire: { checks: PullRequestCheck[]; members: SharedFailureMember[] } = JSON.parse(
  readFileSync(resolve(process.cwd(), "tests/fixtures/pull-request-checks.json"), "utf8"),
);

interface PageData {
  dashboard: Dashboard | null;
  detail: PullRequestDetail | null;
  shared: SharedFailureIndex | null;
  job: JobDetail | null;
}

const pages = await withSSR(async (vite) => {
  const { fixture } = await vite.ssrLoadModule("/src/hooks/useData.ts") as { fixture: PageData };
  const { DashboardPage } = await vite.ssrLoadModule("/src/pages/DashboardPage.tsx") as {
    DashboardPage: ComponentType;
  };
  const { PullRequestDetailPage } = await vite.ssrLoadModule("/src/pages/PullRequestDetailPage.tsx") as {
    PullRequestDetailPage: ComponentType;
  };
  const { SharedFailurePage } = await vite.ssrLoadModule("/src/pages/SharedFailurePage.tsx") as {
    SharedFailurePage: ComponentType;
  };
  const { TestDetailPage } = await vite.ssrLoadModule("/src/pages/TestDetailPage.tsx") as {
    TestDetailPage: ComponentType;
  };
  const { ManifestContext } = await vite.ssrLoadModule("/src/hooks/useManifest.ts") as {
    ManifestContext: Context<Manifest | null>;
  };
  const { defaultTheme } = await vite.ssrLoadModule("/src/theme/index.ts") as { defaultTheme: Theme };
  return { fixture, DashboardPage, PullRequestDetailPage, SharedFailurePage, TestDetailPage, ManifestContext, defaultTheme };
}, [{
  name: "dashboard-wire-data",
  load(id) {
    if (!id.endsWith("/src/hooks/useData.ts")) return;
    return `
      export const fixture = { dashboard: null, detail: null, shared: null, job: null };
      const result = (data) => ({ data, loading: data === null, error: null });
      export const useDashboard = () => result(fixture.dashboard);
      export const useFlakinessReport = () => result(null);
      export const usePullRequestDetail = () => result(fixture.detail);
      export const useSharedFailures = () => result(fixture.shared);
      export const useJobDetail = () => result(fixture.job);
    `;
  },
}]);

const manifest: Manifest = {
  id: "example",
  name: "Example",
  discovery: { source: "testgrid", include_presubmits: true },
  storage: { provider: "gcs", bucket: "logs" },
  branding: {
    title: "Example",
    base_path: "/",
    site_url: "https://example.test",
    source_repo: { owner: "example", name: "project" },
  },
  pull_requests: { enabled: true },
};

const publishedJob: JobSummary = {
  name: wire.checks[0].job_name,
  job_id: wire.checks[0].job_id,
  job_type: "presubmit",
  repo: "example/project",
  tab_name: "",
  category: "",
  branch: "main",
  description: "",
  minimum_interval: "",
  timeout: "",
  config_file: "",
  current_status: "RUNNING",
  overall_status: "FAILING",
  last_run: null,
  recent_runs: [],
  pass_rate_recent: 0,
};

function resetData(): void {
  pages.fixture.dashboard = { generated_at: "", jobs: [publishedJob] };
  pages.fixture.detail = {
    number: 7,
    title: "Update project",
    author: "octocat",
    repo: "example/project",
    base_ref: "main",
    head_sha: "1234567",
    html_url: "https://github.com/example/project/pull/7",
    created_at: "2026-08-17T08:00:00Z",
    updated_at: "2026-08-17T10:00:00Z",
    generated_at: "2026-08-17T10:00:00Z",
    ci_state: "FAILING",
    checks_observed: 3,
    checks_failing: 1,
    failing_tests: 0,
    checks: wire.checks,
  };
  pages.fixture.shared = {
    generated_at: "2026-08-17T10:00:00Z",
    repo: "example/project",
    failures: [{
      id: "shared",
      base_ref: "main",
      job_name: publishedJob.name,
      job_id: publishedJob.job_id,
      test_name: "TestA",
      pull_requests: wire.members,
      oldest_build_started: wire.members[1].started,
      newest_build_started: wire.members[0].started,
      escalatable: true,
    }],
  };
  pages.fixture.job = null;
}

function renderPage(
  component: ComponentType,
  path: string,
  entry: string,
  config: Manifest = manifest,
): string {
  return renderToStaticMarkup(
    createElement(ThemeProvider, { theme: pages.defaultTheme },
      createElement(pages.ManifestContext.Provider, { value: config },
        createElement(MemoryRouter, { initialEntries: [entry] },
          createElement(Routes, null,
            createElement(Route, { path, element: createElement(component) }),
          ),
        ),
      ),
    ),
  );
}

test("empty dashboard points to supported discovery settings", () => {
  resetData();
  pages.fixture.dashboard!.jobs = [];
  const html = renderPage(pages.DashboardPage, "/", "/");
  assert.match(html, /No jobs available yet/);
  assert.match(html, /discovery\.testgrid_dashboard/);
  assert.match(html, /bucket discovery settings/);
  assert.match(html, /project\.yaml/);
  assert.doesNotMatch(html, /testgrid\.dashboard/);
});

test("Go-serialized checks stay running or completed in the pull request UI", () => {
  resetData();
  assert.equal(Object.hasOwn(wire.checks[0], "finished"), false);
  assert.deepEqual(wire.checks.map(checkState), ["RUNNING", "FAILING", "PASSING"]);
  assert.deepEqual(wire.checks.map(checkStatusLabel), ["Running", "Failed", "Passed"]);
  assert.equal(checkSummaryLine(wire.checks[0]), "Still running");

  const html = renderPage(pages.PullRequestDetailPage, "/pull-requests/:number", "/pull-requests/7");
  assert.match(html, /Still running · build 101 · Running/);
  assert.match(html, /Failed without reporting a failing test · build 100 · 30m/);
  assert.match(html, /Passed · build 99 · 30m/);
  assert.doesNotMatch(html, /0001-01-01|· -\d+s/);

  assert.equal(Object.hasOwn(wire.members[0], "finished"), false);
  const shared = pages.fixture.shared!.failures[0];
  assert.equal(evidenceMember(shared)?.number, 2);
  assert.equal(sharedFailureAnalyzable({ ...shared, pull_requests: [wire.members[0]] }), false);
});

for (const state of ["published", "unpublished", "loading", "disabled"] as const) {
  test(`presubmit links on both detail pages: ${state}`, () => {
    resetData();
    if (state === "unpublished") {
      pages.fixture.dashboard!.jobs = [{ ...publishedJob, job_id: "another/job" }];
    } else if (state === "loading") {
      pages.fixture.dashboard = null;
    }
    const config = {
      ...manifest,
      discovery: { ...manifest.discovery, include_presubmits: state !== "disabled" },
    };
    const href = `href="${jobPath(publishedJob.job_id)}"`;
    for (const html of [
      renderPage(pages.PullRequestDetailPage, "/pull-requests/:number", "/pull-requests/7", config),
      renderPage(pages.SharedFailurePage, "/pull-requests/shared/:id", "/pull-requests/shared/shared", config),
    ]) {
      assert.equal(html.includes(href), state === "published", state);
      assert.match(html, /pull-e2e/);
      assert.doesNotMatch(html, /Failed to load|Loading dashboard/);
    }
  });
}

test("test evidence preserves published links and controller artifact citations", () => {
  resetData();
  const base = "https://artifacts.example.test/build/100";
  const source = "https://github.com/example/project/blob/abc/test.go#L10";
  const controllerPath = "artifacts/clusters/management/logs/example-controller.log";
  pages.fixture.job = {
    name: "periodic-example",
    job_id: "example/periodic-example",
    job_type: "periodic",
    repo: "example/project",
    runs: [{
      build_id: "100",
      job_name: "periodic-example",
      started: "2026-08-17T09:00:00Z",
      finished: "2026-08-17T09:30:00Z",
      passed: false,
      result: "FAILURE",
      duration_seconds: 1800,
      commit: "abc",
      prow_url: `${base}/prow`,
      web_url: base,
      build_log_url: `${base}/build-log.txt`,
      junit_urls: [`${base}/junit.xml`],
      test_cases: [{
        name: "TestA",
        status: "failed",
        duration_seconds: 30,
        failure_location_url: source,
        ai_analysis: {
          generated_at: "2026-08-17T10:00:00Z",
          root_cause: `Reconciliation failed in \`${controllerPath}\`.`,
          severity: "High",
          suggested_fix: "Correct the reconciliation check in `test.go`.",
          relevant_files: [controllerPath, "test.go"],
          file_links: { "test.go": source },
          evidence_citations: [{
            path: controllerPath,
            line_start: 10,
            line_end: 10,
            quote: "reconciliation failed",
          }],
        },
      }],
      tests_total: 1,
      tests_passed: 0,
      tests_failed: 1,
      tests_skipped: 0,
    }],
  };
  const html = renderPage(pages.TestDetailPage, "/job/:jobName/test/:testName", "/job/example/test/TestA");
  for (const url of [
    base, `${base}/prow`, `${base}/build-log.txt`, `${base}/junit.xml`, source,
    `${base}/${controllerPath}`,
  ]) {
    assert.ok(html.includes(`href="${url}"`), url);
  }
  assert.match(html, /reconciliation failed/);
  assert.match(html, /1 citation from 1 artifact/);
  assert.match(html, /3 links/);
  assert.doesNotMatch(html, /artifacts\/clusters\/bootstrap\/logs\/|Controller logs/);
});
