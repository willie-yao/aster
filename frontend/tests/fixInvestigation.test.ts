import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, test } from "node:test";
import { createElement, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import {
  analysisChatDefaultComposerMode,
  buildFixInvestigationQuestion,
  limitAnalysisChatQuestion,
  maxFixInvestigationOutcomeBytes,
  sendAnalysisChatMessage,
  streamAnalysisChatMessage,
} from "../src/lib/analysisChat.js";
import type { AnalysisChatReference, AnalysisChatSession } from "../src/types/analysisChat.js";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";
import { withSSR } from "./helpers/ssr.js";

const encoder = new TextEncoder();
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

const { AnalysisChat, AuthContext, CapabilitiesContext, defaultTheme } = await withSSR(async (vite) => ({
  ...await vite.ssrLoadModule("/src/components/AnalysisChat.tsx") as {
    AnalysisChat: (props: { analysisRef: AnalysisChatReference; fileCtx: Record<string, unknown> }) => ReturnType<typeof createElement>;
  },
  ...await vite.ssrLoadModule("/src/hooks/useAuth.ts") as { AuthContext: Context<AuthState> },
  ...await vite.ssrLoadModule("/src/hooks/useCapabilities.ts") as { CapabilitiesContext: Context<Capabilities> },
  ...await vite.ssrLoadModule("/src/theme/index.ts") as { defaultTheme: Theme },
}));

const testRef: AnalysisChatReference = { scope: "test", job_id: "job", build_id: "123", test_name: "failed", junit_file: "junit.xml" };
const causeRef: AnalysisChatReference = { scope: "cause", job_id: "job", pattern_id: "pattern", pattern_hash: "hash", causal_group_id: "cause", causal_group_hash: "cause-hash" };
const chatOnly: Capabilities = { mode: "server", features: { actions: false, analysis_chat: true } };
const operator: AuthState = { status: "authenticated", login: "operator", mode: "oauth", signIn: () => {}, signOut: async () => {} };

function render(ref: AnalysisChatReference, capabilities = chatOnly, auth = operator): string {
  return renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme },
    createElement(AuthContext.Provider, { value: auth },
      createElement(CapabilitiesContext.Provider, { value: capabilities },
        createElement(AnalysisChat, { analysisRef: ref, fileCtx: {} })))));
}

test("fresh and prepared-only conversations default to guided investigation", () => {
  const empty: AnalysisChatSession = {
    id: "session", analysis: causeRef, messages: [], turns_used: 0, max_turns: 10,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z",
  };
  assert.equal(analysisChatDefaultComposerMode(null), "guided");
  assert.equal(analysisChatDefaultComposerMode(empty), "guided");
  assert.equal(analysisChatDefaultComposerMode({ ...empty, messages: [{
    role: "assistant", prepared: true, request_id: "prepared", content: "Prepared finding", created_at: empty.created_at,
  }] }), "guided");
});

test("restored operator conversations and interrupted attempts default to custom questions", () => {
  const empty: AnalysisChatSession = {
    id: "session", analysis: testRef, messages: [], turns_used: 0, max_turns: 10,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z",
  };
  assert.equal(analysisChatDefaultComposerMode({ ...empty, messages: [{
    role: "user", content: "Check my hypothesis", created_at: empty.created_at,
  }] }), "question");
  for (const outcome of ["pending", "succeeded", "failed", "cancelled", "timed_out", "unknown"] as const) {
    assert.equal(analysisChatDefaultComposerMode({ ...empty, attempts: [{ request_id: "attempt", outcome }] }), "question", outcome);
  }
  assert.equal(analysisChatDefaultComposerMode({ ...empty, active: {
    request_id: "active", phase: "investigating", updated_at: empty.updated_at,
  } }), "question");
});

test("blank fix investigations ask the agent to infer and label a goal without granting policy authority", () => {
  const question = buildFixInvestigationQuestion("");
  assert.ok(question.startsWith("Investigate a fix for this failure."));
  assert.match(question, /No desired outcome supplied\. Infer and clearly label the goal/);
  assert.doesNotMatch(question, /Desired outcome:/);
  assert.match(question, /An inferred goal does not authorize policy changes/);
  assert.match(question, /resolves the cause, mitigates impact, or improves diagnostics/);
  assert.match(question, /whether the original failure would still occur/);
  assert.match(question, /how to test the outcome/);
  assert.doesNotMatch(question, /do not generate|do not edit|read-only/i);
  assert.equal(buildFixInvestigationQuestion(" \n\t"), question);
});

test("an explicit desired outcome keeps the operator's complete intent", () => {
  const goal = "Prevent publication delays from failing this test.\n保持 coverage 😀";
  const question = buildFixInvestigationQuestion(`  ${goal}  `);
  assert.ok(question.endsWith(`Desired outcome: ${goal}`));
  assert.doesNotMatch(question, /Infer and clearly label/);
  assert.ok(encoder.encode(question).byteLength <= 4096);
});

test("the outcome budget includes the entire UTF-8 question and rejects excess intent", () => {
  const exact = "x".repeat(maxFixInvestigationOutcomeBytes - 4) + "😀";
  assert.equal(encoder.encode(buildFixInvestigationQuestion(exact)).byteLength, 4096);
  assert.throws(() => buildFixInvestigationQuestion(exact + "x"), RangeError);
  assert.throws(() => buildFixInvestigationQuestion("😀".repeat(maxFixInvestigationOutcomeBytes)), RangeError);
  const limited = limitAnalysisChatQuestion("😀".repeat(maxFixInvestigationOutcomeBytes), maxFixInvestigationOutcomeBytes);
  assert.equal(encoder.encode(limited).byteLength, Math.floor(maxFixInvestigationOutcomeBytes / 4) * 4);
  assert.ok(encoder.encode(buildFixInvestigationQuestion(limited)).byteLength <= 4096);
  assert.equal(limitAnalysisChatQuestion("x".repeat(5000)).length, 4096);
});

test("test, build, and cause chats default to guided investigation without Fix capabilities or source hints", () => {
  for (const ref of [testRef, { scope: "test", job_id: "job", build_id: "123", test_name: "build", source: "build" } as AnalysisChatReference, causeRef]) {
    const html = render(ref);
    assert.match(html, /role="tab"[^>]*aria-selected="true"[^>]*>Guided investigation</);
    assert.match(html, /role="tab"[^>]*aria-selected="false"[^>]*>Custom question<\/button>/);
    assert.match(html, /Desired outcome \(optional\)/);
    assert.match(html, />Start investigation<\/button>/);
    assert.doesNotMatch(html, /Ask for evidence, test another cause/);
    assert.doesNotMatch(html, /Use this finding in a fix proposal/);
  }
  assert.doesNotMatch(render({ scope: "pattern", job_id: "job", pattern_id: "mixed", pattern_hash: "hash" }), /Guided investigation/);
  assert.doesNotMatch(render(testRef, { mode: "static", features: { actions: false } }), /Guided investigation/);
  assert.doesNotMatch(render(testRef, chatOnly, { ...operator, status: "anonymous" }), /Guided investigation/);
});

test("changing composer modes cannot send or erase either draft", () => {
  const source = readFileSync(resolve("src/components/AnalysisChat.tsx"), "utf8");
  const select = source.slice(source.indexOf("function selectComposerMode("), source.indexOf("async function submit("));
  assert.match(source, /investigationLocked = composerLocked \|\| resetting \|\| continueMode/);
  assert.match(select, /if \(investigationLocked\) return/);
  assert.doesNotMatch(select, /submit\(|fetch\(|setQuestion\(|setDesiredOutcome\(/);
  assert.match(source, /aria-disabled=\{investigationLocked \|\| undefined\}/);
  assert.match(source, /pendingTurn\?\.question \?\? draftQuestion/);
  assert.match(source, /question: value/);
  assert.match(source, /value=\{fixInvestigation \? desiredOutcome : question\}/);
  assert.match(source, /"Continue investigation"/);
  assert.match(source, /"Send question"/);
});

test("fix investigations remain ordinary HTTP and streaming chat questions", async () => {
  const question = buildFixInvestigationQuestion("Keep the test passing when publication lags.");
  const completed: AnalysisChatSession = {
    id: "session", created_by: "operator", analysis: testRef, messages: [], turns_used: 0, max_turns: 10,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z",
  };
  const requests: Array<{ url: string; body: unknown; requestID: string | null }> = [];
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), body: JSON.parse(String(init?.body)), requestID: new Headers(init?.headers).get("Idempotency-Key") });
    if (String(input).endsWith("/stream")) {
      return new Response(`event: session\ndata: ${JSON.stringify(completed)}\n\n`, { headers: { "Content-Type": "text/event-stream" } });
    }
    return Response.json(completed);
  };
  await sendAnalysisChatMessage("session", question, "request-http");
  await streamAnalysisChatMessage("session", question, "request-stream", () => {});
  assert.deepEqual(requests.map((request) => request.body), [{ message: question }, { message: question }]);
  assert.deepEqual(requests.map((request) => request.requestID), ["request-http", "request-stream"]);
  assert.ok(requests.every((request) => request.url.startsWith("/api/analysis-chat/sessions/session/")));
});
