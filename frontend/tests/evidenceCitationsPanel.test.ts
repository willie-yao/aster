import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import type { AIAnalysis, EvidenceCitation } from "../src/types/dashboard.js";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { AiAnalysisPanel } = (await vite.ssrLoadModule("/src/components/AiAnalysisPanel.tsx")) as {
  AiAnalysisPanel: (props: {
    analysis: AIAnalysis;
    fileCtx: Record<string, unknown>;
    buildWebURL?: string;
    appearance?: "default" | "detail";
  }) => ReturnType<typeof createElement>;
};
const { CapabilitiesContext } = (await vite.ssrLoadModule("/src/hooks/useCapabilities.ts")) as {
  CapabilitiesContext: React.Context<Capabilities>;
};
const { AuthContext } = (await vite.ssrLoadModule("/src/hooks/useAuth.ts")) as {
  AuthContext: React.Context<AuthState>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
await vite.close();

const capabilities: Capabilities = { mode: "static", features: { actions: false } };
const anonymous: AuthState = {
  status: "anonymous",
  login: null,
  mode: "oauth",
  signIn: () => {},
  signOut: async () => {},
};

function analysisWith(citations?: EvidenceCitation[]): AIAnalysis {
  return {
    generated_at: "2026-08-19T00:00:00Z",
    root_cause: "The controller overwrote its ownership annotation before the delete was confirmed.",
    severity: "High",
    suggested_fix: "Write the annotation only after the delete is confirmed.",
    relevant_files: ["azure/services/securitygroups/securitygroups.go"],
    evidence_citations: citations,
  };
}

function render(
  analysis: AIAnalysis,
  appearance: "default" | "detail" = "detail",
  buildWebURL?: string,
): string {
  return renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: defaultTheme },
      createElement(
        MemoryRouter,
        null,
        createElement(
          CapabilitiesContext.Provider,
          { value: capabilities },
          createElement(
            AuthContext.Provider,
            { value: anonymous },
            createElement(AiAnalysisPanel, { analysis, fileCtx: {}, appearance, buildWebURL }) as ReactNode,
          ),
        ),
      ),
    ),
  );
}

function text(html: string): string {
  return html.replace(/<style\b[^>]*>[\s\S]*?<\/style>/gu, "").replace(/<[^>]+>/gu, " ");
}

test("an analysis without citations shows no evidence section at all", () => {
  // Every analysis cached before this feature has no citations, so the panel
  // must look exactly as it did rather than gain an empty header.
  for (const analysis of [analysisWith(undefined), analysisWith([])]) {
    assert.doesNotMatch(text(render(analysis)), /Evidence/u);
  }
});


test("a cited artifact quote is rendered verbatim next to its location", () => {
  const html = render(analysisWith([
    {
      path: "build-log.txt",
      line_start: 412,
      line_end: 418,
      quote: "Error: failed to reconcile AzureMachine: context deadline exceeded",
    },
  ]));
  const rendered = text(html);
  assert.match(rendered, /Evidence/u);
  assert.match(rendered, /build-log\.txt/u);
  assert.match(rendered, /L412-L418/u);
  assert.match(rendered, /failed to reconcile AzureMachine: context deadline exceeded/u);
});

test("the evidence section reads as a sibling of the other analysis sections", () => {
  const html = render(analysisWith([
    { path: "build-log.txt", line_start: 1, line_end: 1, quote: "boom" },
  ]));
  // Detail appearance renders each block as a <section>; evidence must join
  // them so it inherits the shared separator treatment.
  const sections = [...html.matchAll(/<section class="([^"]+)"/gu)];
  assert.ok(sections.length >= 4, `expected evidence to add a section, saw ${sections.length}`);
});

test("only the first citations are shown until the rest are expanded", () => {
  const citations: EvidenceCitation[] = Array.from({ length: 5 }, (_unused, index) => ({
    path: `artifact-${index}.txt`,
    line_start: index + 1,
    line_end: index + 1,
    quote: `unique-quote-marker-${index}`,
  }));
  const rendered = text(render(analysisWith(citations)));
  assert.match(rendered, /Show 3 more/u);
  assert.match(rendered, /5 citations from 5 artifacts/u);
});

test("a quote keeps its indentation instead of collapsing to one line", () => {
  const html = render(analysisWith([
    { path: "build-log.txt", line_start: 3, line_end: 4, quote: "    indented detail\n    second line" },
  ]));
  // pre-wrap is what preserves the leading spaces that make a log line legible.
  assert.match(html, /white-space:pre-wrap/u);
});

test("the section renders in the compact appearance too", () => {
  const rendered = text(render(
    analysisWith([{ path: "build-log.txt", line_start: 7, line_end: 7, quote: "compact-quote" }]),
    "default",
  ));
  assert.match(rendered, /Evidence/u);
  assert.match(rendered, /compact-quote/u);
});

test("a cited artifact links into the build when a build is in scope", () => {
  const html = render(
    analysisWith([
      { path: "artifacts/junit.e2e_suite.1.xml", line_start: 213, line_end: 214, quote: "status=failed" },
    ]),
    "detail",
    "https://gcsweb.example/gcs/bucket/logs/job/1234",
  );
  assert.match(
    html,
    /href="https:\/\/gcsweb\.example\/gcs\/bucket\/logs\/job\/1234\/artifacts\/junit\.e2e_suite\.1\.xml"/u,
  );
  // Opening an artifact must not hand the origin to the opened tab.
  assert.match(html, /rel="noopener noreferrer"/u);
});

test("cited paths stay plain text when no build is in scope", () => {
  const html = render(
    analysisWith([{ path: "build-log.txt", line_start: 1, line_end: 1, quote: "boom" }]),
  );
  const rendered = text(html);
  assert.match(rendered, /build-log\.txt/u);
  assert.doesNotMatch(html, /<a[^>]*build-log\.txt/u);
});
