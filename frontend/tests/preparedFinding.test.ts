import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve as resolvePath } from "node:path";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";
import type { CauseAnalysisChatReference } from "../src/types/analysisChat.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { AnalysisChat } = (await vite.ssrLoadModule("/src/components/AnalysisChat.tsx")) as {
  AnalysisChat: (props: {
    analysisRef: CauseAnalysisChatReference;
    fileCtx: { builds: Record<string, never>; fileLinks: Record<string, string> };
    preparedFinding?: boolean;
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

const chatCapable: Capabilities = {
  mode: "server",
  features: { actions: true, analysis_chat: true },
  auth: { mode: "oauth" },
};

const operator: AuthState = {
  status: "authenticated",
  login: "willie-yao",
  mode: "oauth",
  signIn: () => {},
  signOut: async () => {},
};

const visitor: AuthState = { ...operator, status: "anonymous", login: null };

const causeRef: CauseAnalysisChatReference = {
  scope: "cause",
  job_id: "periodic-capz-e2e-main",
  pattern_id: "pattern-1",
  pattern_hash: "pattern-hash",
  causal_group_id: "g1",
  causal_group_hash: "h1",
};

function render(preparedFinding: boolean, auth: AuthState = operator): string {
  const tree: ReactNode = createElement(
    ThemeProvider,
    { theme: defaultTheme },
    createElement(
      MemoryRouter,
      null,
      createElement(
        CapabilitiesContext.Provider,
        { value: chatCapable },
        createElement(
          AuthContext.Provider,
          { value: auth },
          createElement(AnalysisChat, {
            analysisRef: causeRef,
            fileCtx: { builds: {}, fileLinks: {} },
            preparedFinding,
          }),
        ),
      ),
    ),
  );
  return renderToStaticMarkup(tree);
}

function source(file: string): string {
  return readFileSync(resolvePath(process.cwd(), file), "utf8");
}

test("a collapsed cause says a prepared finding is waiting", () => {
  const html = render(true);

  assert.match(html, /Finding ready/);
  // The wording must not claim a conclusion. A prepared finding is one
  // pre-computed first answer and may challenge the published cause.
  assert.doesNotMatch(html, /Investigated/);
  assert.match(html, /may challenge the published root cause/);
});

test("a cause with nothing waiting stays unmarked", () => {
  const html = render(false);

  assert.match(html, /Investigate cause/);
  assert.doesNotMatch(html, /Finding ready/);
});

test("an anonymous visitor sees no marker on a control that only prompts sign-in", () => {
  assert.doesNotMatch(render(true, visitor), /Finding ready/);
});

test("the marker is resolved read-only, never by creating a shared session", () => {
  const banner = source("src/components/PatternBanner.tsx");
  assert.match(banner, /lookupPreparedAnalysisChatFindings/);
  assert.doesNotMatch(banner, /createPreparedAnalysisChatSession/);

  // A cause card unmounts its chat panel when it folds, so what the create
  // actually found has to be reported to the parent, which survives that.
  assert.match(banner, /recordPrepared/);
  const chat = source("src/components/AnalysisChat.tsx");
  assert.match(chat, /onPreparedResolved\?\.\(Boolean\(created\)\);/);
});
