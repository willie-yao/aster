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
  model: "test",
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

test("analysis sections render as section elements so the rule can scope past other siblings", () => {
  const html = render();
  const sections = html.match(/<section\b/gu) ?? [];

  // Root cause, Suggested remediation, Related files.
  assert.equal(sections.length, 3);

  // The status row and the chat panel are divs, so scoping the rule to the
  // section element is what keeps "first" meaning the first labelled block
  // rather than the first child.
  assert.doesNotMatch(html, /<section[^>]*>\s*<div[^>]*class="[^"]*MuiStack/u);
});

test("every section after the first carries a rule, and the first does not", () => {
  const html = render();

  // All three sections share one generated class, so the differentiation can
  // only come from the selector. If a future change moved the border onto the
  // element itself, the first section would gain a stray rule.
  const classes = [...html.matchAll(/<section class="([^"]+)"/gu)].map((match) => match[1]);
  assert.equal(classes.length, 3);
  assert.equal(new Set(classes).size, 1);

  const sectionClass = classes[0].split(/\s+/u).find((name) => name.startsWith("css-"));
  assert.ok(sectionClass, "expected an emotion class on the section");

  const rule = new RegExp(`\\.${sectionClass}:not\\(:first-of-type\\)\\{[^}]*border-top`, "u");
  assert.match(html, rule);
});

test("severity is not repeated when the surrounding header already states it", () => {
  // Callers with no header of their own keep the chip as their only severity
  // signal, so the default must still render it.
  assert.match(render(), /Severity: High/u);
  assert.match(render(false), /Severity: High/u);

  assert.doesNotMatch(render(true), /Severity: High/u);
});
