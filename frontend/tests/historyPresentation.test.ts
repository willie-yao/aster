import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { createElement, type ComponentType, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ThemeProvider } from "@mui/material/styles";
import { MemoryRouter } from "react-router-dom";
import { withSSR } from "./helpers/ssr.js";
import type { AnalysisChatSession } from "../src/types/analysisChat.js";
import type { NavDestination } from "../src/lib/navigation.js";

const { AnalysisChatTranscript, AnalysisChatHistory, NavRail, NavBottomBar, defaultTheme } = await withSSR(async (vite) => {
  const { AnalysisChatTranscript } = await vite.ssrLoadModule("/src/components/AnalysisChat.tsx") as {
    AnalysisChatTranscript: ComponentType<{ session: AnalysisChatSession; historical?: boolean; chatFixEnabled?: boolean; fixEligible?: boolean }>;
  };
  const { AnalysisChatHistory } = await vite.ssrLoadModule("/src/components/AnalysisChatHistory.tsx") as {
    AnalysisChatHistory: ComponentType<{ jobID: string; scope: string; refreshKey: string }>;
  };
  const { NavRail, NavBottomBar } = await vite.ssrLoadModule("/src/components/NavRail.tsx") as {
    NavRail: ComponentType<{ destinations: NavDestination[]; homeLabel: string }>;
    NavBottomBar: ComponentType<{ destinations: NavDestination[] }>;
  };
  const { defaultTheme } = await vite.ssrLoadModule("/src/theme/index.ts") as typeof import("../src/theme/index.js");
  return { AnalysisChatTranscript, AnalysisChatHistory, NavRail, NavBottomBar, defaultTheme };
});

function render(node: ReactNode, path = "/"): string {
  return renderToStaticMarkup(createElement(
    ThemeProvider, { theme: defaultTheme },
    createElement(MemoryRouter, { initialEntries: [path] }, node),
  ));
}

const session: AnalysisChatSession = {
  id: "saved", analysis: { job_id: "job", build_id: "123", test_name: "Original test" },
  created_at: "2026-09-01T00:00:00Z", updated_at: "2026-09-01T00:00:00Z",
  expires_at: "2026-09-02T00:00:00Z", archived: true, read_only: true,
  turns_used: 1, max_turns: 12,
  messages: [{
    role: "assistant", request_id: "prepared", prepared: true, assessment: "challenges",
    created_at: "2026-09-01T00:00:00Z", content: "An earlier finding, not the current conclusion.",
    citations: [{ path: "artifacts/original.log", line_start: 10, line_end: 12, quote: "original evidence" }],
  }],
};

test("historical transcripts preserve evidence without offering current Fix actions", () => {
  const html = render(createElement(AnalysisChatTranscript, {
    session, historical: true, chatFixEnabled: true, fixEligible: true,
  }));
  assert.match(html, /original evidence snapshot/);
  assert.match(html, /An earlier finding, not the current conclusion/);
  assert.match(html, /artifacts\/original\.log/);
  assert.match(html, /lines 10-12/);
  assert.match(html, /original evidence/);
  assert.match(html, /Evidence challenges it/);
  assert.doesNotMatch(html, /before opening a Fix proposal|Use this finding in a fix proposal/);
});

test("current transcripts retain their existing Fix affordance", () => {
  const html = render(createElement(AnalysisChatTranscript, {
    session: { ...session, archived: false, read_only: false }, chatFixEnabled: true, fixEligible: true,
  }));
  assert.match(html, /before opening a Fix proposal/);
  assert.match(html, /Use this finding in a fix proposal/);
});

test("an empty historical transcript explains the absence of messages", () => {
  const html = render(createElement(AnalysisChatTranscript, {
    session: { ...session, messages: [] }, historical: true,
  }));
  assert.match(html, /No messages were saved for this conversation/);
});

test("earlier history links to filtered history and explains its scope", () => {
  const html = render(createElement(AnalysisChatHistory, {
    jobID: "job/name", scope: "cause", refreshKey: "",
  }));
  assert.match(html, /aria-expanded="false"/);
  assert.match(html, /Earlier conversations/);
  assert.match(html, /View history/);
  assert.match(html, /href="\/investigations\?job_id=job%2Fname&amp;scope=cause"/);
  assert.doesNotMatch(html, /Loading conversation history/);

  const history = readFileSync("src/components/AnalysisChatHistory.tsx", "utf8");
  assert.match(history, /may describe different causes or older evidence/);
});

test("history filters use the shared mobile input treatment and offer clearing", () => {
  const page = readFileSync("src/pages/InvestigationHistoryPage.tsx", "utf8");
  assert.equal(page.match(/\.\.\.filterFieldSx/g)?.length, 2);
  assert.match(page, /Clear filters/);
  assert.match(page, /Evidence builds/);
  assert.match(page, /Comparison build/);
  assert.match(page, /Pinned source/);
  assert.match(page, /Archived · Read-only/);
});

test("new conversation copy only promises history for retained conversations", () => {
  const chat = readFileSync("src/components/AnalysisChat.tsx", "utf8");
  assert.match(chat, /session\?\.history_expires_at\s*\? "The previous conversation is saved, not deleted/);
  assert.match(chat, /read-only for every operator/);
  assert.match(chat, /retention deadline/);
  assert.match(chat, /: "This conversation is not saved in history/);
  assert.match(chat, /without adding it to history/);
  assert.match(chat, /session\?\.history_expires_at \? "Save and start new" : "Archive and start new"/);
  assert.match(chat, /session\?\.messages\.some\(\(message\) => message\.prepared\) &&/);
  assert.match(chat, /prepared finding is also dismissed for every operator/);
});

test("navigation distinguishes the exact current page from a highlighted parent", () => {
  const destinations = [{ id: "overview", to: "/", label: "Overview", title: "Overview", scope: "signal" as const, active: true }];
  for (const path of ["/", "/job/job/test/test"]) {
    for (const node of [
      createElement(NavRail, { destinations, homeLabel: "Aster home" }),
      createElement(NavBottomBar, { destinations }),
    ]) {
      const html = render(node, path);
      assert.match(html, path === "/" ? /aria-current="page"/ : /aria-current="true"/);
      if (path !== "/") assert.doesNotMatch(html, /aria-current="page"/);
    }
  }
});
