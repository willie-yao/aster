import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { createElement, type ComponentType, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ThemeProvider } from "@mui/material/styles";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import { fetchFreshnessDetail, jobFetchFreshnessDetail } from "../src/lib/fetchFreshness.js";
import { normalizeFlakinessReport, type FlakinessReportWire } from "../src/lib/flakinessReport.js";
import type { Manifest } from "../src/types/manifest.js";
import { jobDataFilename } from "../src/lib/utils.js";
import type { Dashboard, JobDetail } from "../src/types/dashboard.js";

// The output package verifies this fixture against the Go writer.
const wire: {
  dashboard: Dashboard;
  detail: JobDetail;
  flakiness: FlakinessReportWire;
  empty_dashboard: Dashboard;
  empty_flakiness: FlakinessReportWire;
} = JSON.parse(readFileSync(resolve(process.cwd(), "tests/fixtures/fetch-freshness.json"), "utf8"));

const pages = await withSSR(async (vite) => {
  const { fixture } = await vite.ssrLoadModule("/src/hooks/useData.ts") as { fixture: Record<string, unknown> };
  const { DashboardPage } = await vite.ssrLoadModule("/src/pages/DashboardPage.tsx") as { DashboardPage: ComponentType };
  const { FlakinessPage } = await vite.ssrLoadModule("/src/pages/FlakinessPage.tsx") as { FlakinessPage: ComponentType };
  const { JobDetailPage } = await vite.ssrLoadModule("/src/pages/JobDetailPage.tsx") as { JobDetailPage: ComponentType };
  const { ManifestContext } = await vite.ssrLoadModule("/src/hooks/useManifest.ts") as { ManifestContext: Context<Manifest | null> };
  const { defaultTheme } = await vite.ssrLoadModule("/src/theme/index.ts") as typeof import("../src/theme/index.js");
  return { fixture, DashboardPage, FlakinessPage, JobDetailPage, ManifestContext, defaultTheme };
}, [{
  name: "fetch-freshness-public-data",
  load(id) {
    if (!id.endsWith("/src/hooks/useData.ts")) return;
    const source = readFileSync(id, "utf8");
    const start = source.indexOf("function useJSON<T>");
    const end = source.indexOf("export function useDashboard");
    return source.slice(0, start) + `
      export const fixture = {};
      function useJSON(path) {
        return { data: fixture[path] ?? null, loading: false, error: null };
      }
    ` + source.slice(end);
  },
}]);

const manifest: Manifest = {
  id: "example", name: "Example", storage: { provider: "gcs", bucket: "logs" },
  branding: { title: "Example", base_path: "/", site_url: "https://example.test", source_repo: { owner: "example", name: "project" } },
};

function render(component: ComponentType, path: string, entry: string): string {
  return renderToStaticMarkup(createElement(ThemeProvider, { theme: pages.defaultTheme },
    createElement(pages.ManifestContext.Provider, { value: manifest },
      createElement(MemoryRouter, { initialEntries: [entry] },
        createElement(Routes, null, createElement(Route, { path, element: createElement(component) })),
      ),
    ),
  ));
}

function setPublicData(dashboard: Dashboard, report: FlakinessReportWire): void {
  pages.fixture["dashboard.json"] = dashboard;
  pages.fixture["flakiness.json"] = report;
  pages.fixture[`jobs/${jobDataFilename("carried-job")}`] = wire.detail;
}

test("public fetch summary survives the flakiness normalizer", () => {
  assert.deepEqual(normalizeFlakinessReport(wire.flakiness).fetch, wire.flakiness.fetch);
  assert.equal(fetchFreshnessDetail({ total: 3, failed: 0, missing: 0 }), null);
  assert.equal(fetchFreshnessDetail(undefined), null);
  assert.equal(jobFetchFreshnessDetail(undefined), null);
  assert.equal(jobFetchFreshnessDetail({ state: "fresh", checked_at: "2026-08-17T10:00:00Z" }), null);
  assert.match(jobFetchFreshnessDetail({ state: "carried_forward", checked_at: "2026-08-17T10:00:00Z" })!, /Last successful fetch unknown/);
});

test("Pages dashboard and trends warn using only published JSON", () => {
  setPublicData(wire.dashboard, wire.flakiness);
  for (const html of [
    render(pages.DashboardPage, "/", "/"),
    render(pages.FlakinessPage, "/flakiness", "/flakiness"),
  ]) {
    assert.match(html, /Published with incomplete job data/);
    assert.match(html, /2 of 2 jobs could not be refreshed/);
    assert.match(html, /1 job shows prior data/);
    assert.match(html, /1 job has no published data/);
    assert.doesNotMatch(html, /private-token|listing unavailable/);
  }
  const overview = render(pages.DashboardPage, "/", "/");
  assert.match(overview, /Carried forward/);
  assert.match(overview, /Passed/);
  const detail = render(pages.JobDetailPage, "/job/:jobName", "/job/carried-job");
  assert.match(detail, /Carried forward/);
  assert.match(detail, /Last successful fetch/);
  assert.match(detail, /Observed status and runs are from prior data/);
});

test("Pages warns when all newly discovered jobs are missing", () => {
  setPublicData(wire.empty_dashboard, wire.empty_flakiness);
  const overview = render(pages.DashboardPage, "/", "/");
  assert.match(overview, /No jobs available yet/);
  assert.match(overview, /Published with incomplete job data/);
  assert.match(overview, /2 jobs have no published data/);
  assert.doesNotMatch(overview, /jobs show prior data/);
  const trends = render(pages.FlakinessPage, "/flakiness", "/flakiness");
  assert.match(trends, /Published with incomplete job data/);
  assert.match(trends, /2 jobs have no published data/);
});

test("successful recovery clears public warnings and carried-forward captions", () => {
  setPublicData({
    ...wire.dashboard,
    fetch: { total: 1, failed: 0, missing: 0 },
    jobs: wire.dashboard.jobs.map((job) => ({ ...job, fetch: { state: "fresh", checked_at: "2026-08-17T11:00:00Z", last_success_at: "2026-08-17T11:00:00Z" } })),
  }, { ...wire.flakiness, fetch: { total: 1, failed: 0, missing: 0 } });
  for (const html of [
    render(pages.DashboardPage, "/", "/"),
    render(pages.FlakinessPage, "/flakiness", "/flakiness"),
  ]) {
    assert.doesNotMatch(html, /Published with incomplete job data|Carried forward/);
  }
});
