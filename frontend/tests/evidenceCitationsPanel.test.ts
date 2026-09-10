import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import type { AIAnalysis, EvidenceCitation } from "../src/types/dashboard.js";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";

const { AiAnalysisPanel, CapabilitiesContext, AuthContext, defaultTheme } = await withSSR(async (vite) => {
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
  return { AiAnalysisPanel, CapabilitiesContext, AuthContext, defaultTheme };
});

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

function subtree(html: string, start: number, tag: "div" | "section") {
  assert.ok(start >= 0, `missing ${tag} anchor`);
  const opening = new RegExp(`^<${tag}\\b[^>]*>`, "u").exec(html.slice(start));
  assert.ok(opening, `expected ${tag} opening at ${start}`);
  const tags = new RegExp(`</?${tag}\\b[^>]*>`, "gu");
  tags.lastIndex = start;
  let depth = 0;
  for (let match = tags.exec(html); match; match = tags.exec(html)) {
    depth += match[0].startsWith("</") ? -1 : 1;
    if (depth === 0) {
      return { start, end: tags.lastIndex, html: html.slice(start, tags.lastIndex), opening: opening[0] };
    }
  }
  assert.fail(`unclosed ${tag} at ${start}`);
}

function evidenceSection(html: string) {
  const sections = [...html.matchAll(/<section\b[^>]*class="[^"]*\bbriefing-section\b[^"]*"[^>]*>/gu)]
    .map((match) => subtree(html, match.index, "section"));
  const evidence = sections.filter((section) => /<h3\b[^>]*>Evidence<\/h3>/u.test(section.html));
  assert.equal(evidence.length, 1, "missing or duplicated Evidence section");
  return { evidence: evidence[0], sections };
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
  const { evidence, sections } = evidenceSection(html);
  const sectionStarts = new Set(sections.map((section) => section.start));
  const parents = new Map<number, number>();
  const ancestors: Array<{ tag: string; start: number }> = [];
  // React SSR self-closes void elements; style contents are not markup.
  const tags = /<style\b[^>]*>[\s\S]*?<\/style>|<(\/?)([a-z][a-z0-9]*)\b[^>]*>/gu;
  for (const match of html.matchAll(tags)) {
    const [, closing, tag] = match;
    if (!tag) continue;
    if (closing) {
      assert.equal(ancestors.pop()?.tag, tag, "unbalanced rendered markup");
      continue;
    }
    if (sectionStarts.has(match.index)) {
      const parent = ancestors.at(-1);
      assert.ok(parent, "briefing section has no rendered parent");
      parents.set(match.index, parent.start);
      if (parents.size === sections.length) break;
    }
    if (!match[0].endsWith("/>")) ancestors.push({ tag, start: match.index });
  }
  assert.equal(parents.size, sections.length, "missing briefing section parent");
  assert.match(text(evidence.html), /boom/u);
  for (const sibling of sections.filter((section) => section !== evidence)) {
    assert.equal(parents.get(sibling.start), parents.get(evidence.start), "briefing sections must share their immediate rendered parent");
    assert.ok(sibling.end <= evidence.start || sibling.start >= evidence.end, "Evidence must be a sibling");
    assert.doesNotMatch(text(sibling.html), /boom/u);
  }
  for (const marker of [
    "The controller overwrote its ownership annotation",
    "Write the annotation only after",
    "azure/services/securitygroups/securitygroups.go",
  ]) {
    assert.ok(sections.some((section) => section !== evidence && text(section.html).includes(marker)), `missing sibling ${marker}`);
    assert.ok(!text(evidence.html).includes(marker), `Evidence incorrectly owns ${marker}`);
  }
});

for (const count of [2, 5]) {
  test(`${count} citations have the correct initial disclosure ownership`, () => {
    const citations: EvidenceCitation[] = Array.from({ length: count }, (_unused, index) => ({
      path: `artifact-${index}.txt`,
      line_start: index + 1,
      line_end: index + 1,
      quote: `unique-quote-marker-${index}`,
    }));
    const html = render(analysisWith(citations));
    const { evidence } = evidenceSection(html);
    assert.ok(text(evidence.html).includes(`${count} citations from ${count} artifacts`));
    const buttons = [...evidence.html.matchAll(/<button\b[^>]*>[\s\S]*?<\/button>/gu)];
    if (count === 2) {
      assert.equal(buttons.length, 0);
      assert.doesNotMatch(evidence.html, /MuiCollapse-root/u);
    } else {
      assert.equal(buttons.length, 1);
      const toggle = buttons[0][0];
      assert.match(toggle, /aria-expanded="false"/u);
      assert.match(text(toggle), /Show 3 more/u);
      const controls = /aria-controls="([^"]+)"/u.exec(toggle);
      assert.ok(controls, "missing disclosure control ID");
      const controlled = [...evidence.html.matchAll(/<div\b[^>]*\sid="([^"]+)"[^>]*>/gu)]
        .filter((match) => match[1] === controls[1]);
      assert.equal(controlled.length, 1, "missing or duplicated controlled subtree");
      const region = subtree(evidence.html, controlled[0].index, "div");
      const collapses = [...evidence.html.matchAll(/<div\b[^>]*class="[^"]*\bMuiCollapse-root\b[^"]*"[^>]*>/gu)];
      assert.equal(collapses.length, 1);
      const collapse = subtree(evidence.html, collapses[0].index, "div");
      assert.ok(collapse.start < region.start && collapse.end > region.end, "controlled subtree must belong to Collapse");
      assert.match(collapse.opening, /\bMuiCollapse-hidden\b/u);
      assert.doesNotMatch(collapse.opening, /\bMuiCollapse-entered\b/u);
      assert.match(collapse.opening, /style="[^"]*height:0px/u);
      const outside = evidence.html.slice(0, collapse.start) + evidence.html.slice(collapse.end);
      for (const [index, citation] of citations.entries()) {
        for (const marker of [citation.path, citation.quote]) {
          assert.ok(marker);
          assert.equal(text(region.html).includes(marker), index >= 2, `${marker} controlled membership`);
          assert.equal(text(outside).includes(marker), index < 2, `${marker} visible membership`);
        }
      }
    }
    const outsideEvidence = html.slice(0, evidence.start) + html.slice(evidence.end);
    for (const citation of citations) {
      for (const marker of [citation.path, citation.quote]) {
        assert.ok(marker);
        assert.equal(text(evidence.html).split(marker).length - 1, 1, `${marker} must occur once in Evidence`);
        assert.ok(!text(outsideEvidence).includes(marker), `${marker} leaked outside Evidence`);
      }
    }
  });
}

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
