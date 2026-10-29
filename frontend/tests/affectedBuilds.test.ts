import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import type { BuildResult } from "../src/types/dashboard.js";

const { AffectedBuildsLedger, defaultTheme } = await withSSR(async (vite) => {
  const { AffectedBuildsLedger } = (await vite.ssrLoadModule("/src/components/AffectedBuildsLedger.tsx")) as {
    AffectedBuildsLedger: (props: { builds: string[]; runs: BuildResult[]; jobID?: string }) => ReturnType<typeof createElement>;
  };
  const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
  return { AffectedBuildsLedger, defaultTheme };
});

function run(build_id: string, started: string, duration_seconds: number): BuildResult {
  return { build_id, started, duration_seconds, passed: false } as BuildResult;
}

function render(builds: string[], runs: BuildResult[]): string {
  return renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme },
    createElement(MemoryRouter, null, createElement(AffectedBuildsLedger, { builds, runs, jobID: "job" }))))
    .replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, "");
}

test("affected builds list newest first with when they ran and how long", () => {
  const started = "2026-10-28T12:10:43Z";
  const day = new Date(started).toLocaleDateString("en-US", { month: "short", day: "numeric" });
  const fullDay = new Date(started).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" });
  const html = render(["9", "100", "11"], [
    run("100", started, 5400),
    run("11", "2026-10-25T12:10:43Z", 1980),
  ]);
  const order = [...html.matchAll(/aria-label="Open affected build (\d+)/g)].map((match) => match[1]);
  assert.deepEqual(order, ["100", "11", "9"]);
  assert.match(html, new RegExp(`<a[^>]*aria-label="Open affected build 100, ${fullDay}, ran 1h 30m"[^>]*>`));
  assert.match(html, /<a[^>]*href="\/job\/job\?run=100"/);
  assert.match(html, new RegExp(`>${day}<[\\s\\S]*?>100<[\\s\\S]*?>1h 30m<`));
  // A build outside the loaded runs keeps its ID but has nothing else to show.
  assert.match(html, /aria-label="Open affected build 9, before the loaded runs"[\s\S]*?>Earlier</);
});

test("affected builds fold past four rows", () => {
  const builds = ["1", "2", "3", "4", "5", "6"];
  const html = render(builds, []);
  assert.equal(html.match(/aria-label="Open affected build/g)?.length, 4);
  assert.match(html, /aria-expanded="false"[^>]*>Show all 6</);
  assert.doesNotMatch(render(builds.slice(0, 4), []), /Show all/);
});
