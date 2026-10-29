import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ThemeProvider, createTheme, type Theme } from "@mui/material/styles";
import { MemoryRouter } from "react-router-dom";
import type { PatternAnalysis } from "../src/types/dashboard.js";
import { withSSR } from "./helpers/ssr.js";

const { AnalysisBriefing, PatternBanner, defaultTheme } = await withSSR(async (vite) => ({
  ...await vite.ssrLoadModule("/src/components/AnalysisBriefing.tsx") as {
    AnalysisBriefing: (props: {
      title: string;
      summary: ReactNode;
      details?: ReactNode;
      actions?: ReactNode;
      mobileNotice?: ReactNode;
      mobileNextStep?: ReactNode;
      mobileSynopsis?: ReactNode;
      collapseDetailsOnMobile?: boolean;
    }) => ReturnType<typeof createElement>;
  },
  ...await vite.ssrLoadModule("/src/components/PatternBanner.tsx") as {
    PatternBanner: (props: { pattern: PatternAnalysis }) => ReturnType<typeof createElement>;
  },
  ...await vite.ssrLoadModule("/src/theme/index.ts") as { defaultTheme: Theme },
}));

function render(element: ReturnType<typeof createElement>, desktop = false): string {
  const theme = createTheme(defaultTheme, {
    components: { MuiUseMediaQuery: { defaultProps: { ssrMatchMedia: () => ({ matches: desktop }) } } },
  });
  return renderToStaticMarkup(createElement(ThemeProvider, { theme }, createElement(MemoryRouter, null, element)));
}

for (const desktop of [false, true]) {
  test(`summary-only ${desktop ? "desktop" : "mobile"} briefing renders once without a disclosure`, () => {
    const html = render(createElement(AnalysisBriefing, {
      title: "Analysis briefing",
      summary: "Unique summary text",
      mobileNotice: "Important notice",
      mobileNextStep: "Suggested next step",
      actions: createElement("button", null, "Keep action"),
    }), desktop);
    assert.equal(html.match(/Unique summary text/g)?.length, 1);
    assert.doesNotMatch(html, /Full analysis|aria-expanded|aria-controls/);
    assert.equal(html.match(/Keep action/g)?.length, 1);
    if (!desktop) {
      assert.equal(html.match(/Important notice/g)?.length, 1);
      assert.equal(html.match(/Suggested next step/g)?.length, 1);
    } else {
      assert.doesNotMatch(html, /Important notice|Suggested next step/);
    }
  });
}

test("populated mobile briefings keep disclosure relationships, details, notices, and actions", () => {
  const html = render(createElement(AnalysisBriefing, {
    title: "Analysis briefing",
    summary: "Complete summary",
    mobileSynopsis: "Short synopsis",
    details: createElement("a", { href: "#evidence" }, "Evidence detail"),
    mobileNotice: "Important notice",
    mobileNextStep: "Suggested next step",
    actions: createElement("button", null, "Keep action"),
  }));
  assert.match(html, /Full analysis/);
  assert.match(html, /aria-expanded="false"/);
  const controls = /aria-controls="([^"]+)"/.exec(html)?.[1];
  assert.ok(controls);
  assert.ok(html.includes(`id="${controls}"`));
  assert.match(html, /<a href="#evidence">Evidence detail<\/a>/);
  assert.match(html, /Complete summary/);
  assert.equal(html.match(/Important notice/g)?.length, 1);
  assert.equal(html.match(/Keep action/g)?.length, 1);
  assert.ok(html.indexOf("Suggested next step") < html.indexOf("Full analysis"));
});

test("desktop and uncollapsed mobile briefings keep useful full details", () => {
  for (const desktop of [false, true]) {
    const html = render(createElement(AnalysisBriefing, {
      title: "Analysis briefing",
      summary: "Complete summary",
      details: "Evidence detail",
      collapseDetailsOnMobile: desktop ? undefined : false,
      actions: createElement("button", null, "Keep action"),
    }), desktop);
    assert.equal(html.match(/Complete summary/g)?.length, 1);
    assert.equal(html.match(/Evidence detail/g)?.length, 1);
    assert.equal(html.match(/Keep action/g)?.length, 1);
    assert.doesNotMatch(html, /aria-expanded|Full analysis/);
  }
});

const summaryOnlyPattern: PatternAnalysis = {
  subject: "job",
  generated_at: "2026-10-29T00:00:00Z",
  builds_analyzed: 2,
  systemic: false,
  confidence: "low",
  recurrence_classification: "insufficient_evidence",
  summary: "Unique pattern summary. Keep this second sentence too.",
};

test("summary-only pattern callers supply no empty details fragment", () => {
  const html = render(createElement(PatternBanner, { pattern: summaryOnlyPattern }));
  assert.equal(html.match(/Unique pattern summary/g)?.length, 1);
  assert.match(html, /Keep this second sentence too/);
  assert.doesNotMatch(html, /Full analysis|aria-expanded|<hr/);
});

test("pattern detail sections still supply a populated mobile disclosure", () => {
  for (const details of [
    { unclassified_builds: ["123"] },
    { relevant_files: ["test/e2e.go"] },
    { systemic: true, shared_root_cause: "Shared root cause" },
    { systemic: true, shared_builds: ["123"] },
    { recurrence_classification: undefined, source_ref: "repo@revision" },
    { recurrence_classification: undefined, systemic: true, suggested_fix: "Change timeout" },
    { causal_groups: [{ builds: ["123"], root_cause: "Cause detail", confidence: "high" as const }] },
    { lifecycle: { state: "recovered" as const, reason: "Recent builds pass" } },
  ]) {
    const html = render(createElement(PatternBanner, { pattern: { ...summaryOnlyPattern, ...details } }));
    assert.match(html, /Full analysis/, JSON.stringify(details));
    assert.match(html, /aria-expanded="false"/, JSON.stringify(details));
  }
});
