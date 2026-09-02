import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import type { AIAnalysis } from "../src/types/dashboard.js";
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
    appearance?: "default" | "detail";
    severityInHeader?: boolean;
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

const analysis: AIAnalysis = {
  generated_at: "2026-08-19T00:00:00Z",
  root_cause: "The controller overwrote its ownership annotation before the delete was confirmed.",
  severity: "High",
  suggested_fix: "Write the annotation only after the delete is confirmed.",
  relevant_files: ["azure/services/securitygroups/securitygroups.go"],
};

function render(severityInHeader?: boolean): string {
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
            createElement(AiAnalysisPanel, {
              analysis,
              fileCtx: {},
              appearance: "detail",
              severityInHeader,
            }) as ReactNode,
          ),
        ),
      ),
    ),
  );
}

// styleSheet joins the contents of every emitted <style> block, so rules can be
// matched without guessing at the boundary between markup and CSS. Matching
// against the raw HTML is what silently made an earlier version of the
// base-rule assertion skip itself.
function styleSheet(html: string): string {
  return [...html.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style>/gu)]
    .map((match) => match[1])
    .join("\n");
}

test("analysis sections render as section elements carrying the marker class", () => {
  const html = render();
  const sections = [...html.matchAll(/<section class="([^"]+)"/gu)].map((match) => match[1]);

  // Root cause, Suggested remediation, Related files.
  assert.equal(sections.length, 3);

  // The sibling rule keys on this class, so every section must carry it or the
  // rule silently stops applying while the selector assertion still passes.
  for (const classes of sections) {
    assert.ok(
      classes.split(/\s+/u).includes("briefing-section"),
      `expected the marker class on every section, got ${JSON.stringify(classes)}`,
    );
  }
});

test("every section preceded by another carries a rule, and the first does not", () => {
  const html = render();
  const css = styleSheet(html);

  // All three sections share one generated class, so the differentiation can
  // only come from the selector.
  const classes = [...html.matchAll(/<section class="([^"]+)"/gu)].map((match) => match[1]);
  assert.equal(new Set(classes).size, 1);

  const sectionClass = classes[0].split(/\s+/u).find((name) => name.startsWith("css-"));
  assert.ok(sectionClass, "expected an emotion class on the section");

  // The rule keys on a preceding sibling section, not on position, so an
  // intervening div or a future sibling section cannot change which block
  // goes unruled. Emotion minifies the combinator, so allow optional spaces.
  assert.match(
    css,
    new RegExp(`\\.briefing-section\\s*~\\s*\\.${sectionClass}\\{[^}]*border-top`, "u"),
  );

  // The unqualified rule must NOT carry a border, or every section including
  // the first would be ruled and the scoped rule would be decorative.
  const base = new RegExp(`(?:^|[};])\\.${sectionClass}\\{([^}]*)\\}`, "mu").exec(css);
  assert.ok(base, `expected an unqualified rule for .${sectionClass}`);
  assert.doesNotMatch(base[1], /border-top/u);
});

test("a non-section sibling precedes the first section, which is why position is not used", () => {
  const html = render();

  // The status row renders before the sections, so :first-child would have
  // ruled "Root cause". This fixture keeps that ordering honest.
  const firstSection = html.indexOf("<section");
  const statusRow = html.indexOf("MuiStack-root");
  assert.notEqual(statusRow, -1, "expected the status row to render");
  assert.ok(statusRow < firstSection, "the status row must precede the first section");
});

test("severity is not repeated when the surrounding header already states it", () => {
  // Callers with no header of their own keep the chip as their only severity
  // signal, so the default must still render it.
  assert.match(render(), /Severity: High/u);
  assert.match(render(false), /Severity: High/u);

  assert.doesNotMatch(render(true), /Severity: High/u);
});
