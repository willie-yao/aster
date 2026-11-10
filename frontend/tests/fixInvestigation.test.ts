import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, test } from "node:test";
import { createElement, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import {
  analysisChatDefaultComposerMode,
  analysisChatPreparedInvestigation,
  buildFixInvestigationQuestion,
  limitAnalysisChatQuestion,
  maxFixInvestigationOutcomeBytes,
  sendAnalysisChatMessage,
  streamAnalysisChatMessage,
} from "../src/lib/analysisChat.js";
import type { AnalysisChatMessage, AnalysisChatReference, AnalysisChatSession } from "../src/types/analysisChat.js";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";
import { withSSR } from "./helpers/ssr.js";

const encoder = new TextEncoder();
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

const { AnalysisChat, PreparedInvestigationReady, AuthContext, CapabilitiesContext, defaultTheme } = await withSSR(async (vite) => ({
  ...await vite.ssrLoadModule("/src/components/AnalysisChat.tsx") as {
    AnalysisChat: (props: { analysisRef: AnalysisChatReference; fileCtx: Record<string, unknown> }) => ReturnType<typeof createElement>;
    PreparedInvestigationReady: (props: {
      message: AnalysisChatMessage;
      fixEnabled: boolean;
      sourceUnavailable: boolean;
      onUseForFix: () => void;
      onDifferentOutcome: () => void;
    }) => ReturnType<typeof createElement>;
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

test("fresh and prepared-only conversations keep guided investigation selected", () => {
  const empty: AnalysisChatSession = {
    id: "session", analysis: causeRef, messages: [], turns_used: 0, max_turns: 10,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z",
  };
  assert.equal(analysisChatDefaultComposerMode(null), "guided");
  assert.equal(analysisChatDefaultComposerMode(empty), "guided");
  const prepared: AnalysisChatSession = { ...empty, messages: [{
    role: "assistant", prepared: true, request_id: "prepared", content: "Prepared investigation", created_at: empty.created_at,
  }], attempts: [{ request_id: "prepared", outcome: "succeeded", turn: 0 }] };
  assert.equal(analysisChatDefaultComposerMode(prepared), "guided");
  assert.equal(analysisChatDefaultComposerMode({ ...prepared, attempts: [] }), "guided");
  assert.equal(analysisChatPreparedInvestigation(prepared), prepared.messages[0]);
  const continued: AnalysisChatSession = { ...prepared, messages: [...prepared.messages, {
    role: "user", actor: "operator", content: "Check another hypothesis", created_at: empty.created_at,
  }] };
  assert.equal(analysisChatDefaultComposerMode(continued), "question");
  assert.equal(analysisChatPreparedInvestigation(continued), null);
});

test("only a nonblank prepared assistant answer completes an untouched investigation", () => {
  const empty: AnalysisChatSession = {
    id: "session", analysis: causeRef, messages: [], turns_used: 0, max_turns: 10,
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-01T01:00:00Z",
  };
  const message = {
    role: "assistant" as const, prepared: true, request_id: "prepared",
    content: "Intended result: Inferred outcome: prevent the observed failure.", created_at: empty.created_at,
  };
  const prepared: AnalysisChatSession = {
    ...empty, messages: [message],
    attempts: [{ request_id: "prepared", outcome: "succeeded", turn: 0 }],
  };
  const original = structuredClone(prepared);
  assert.equal(analysisChatPreparedInvestigation(prepared), message);
  assert.deepEqual(prepared, original);
  assert.equal(analysisChatPreparedInvestigation(null), null);
  assert.equal(analysisChatPreparedInvestigation(empty), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, messages: [{ ...message, content: " \n\t" }] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, messages: [{ ...message, prepared: false }] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, messages: [{ ...message, role: "user" }] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, turns_used: 1 }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, messages: [message, {
    role: "user", content: "Investigate a different outcome", created_at: empty.created_at,
  }] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, attempts: [
    ...prepared.attempts!, { request_id: "operator", outcome: "failed", turn: 1 },
  ] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, attempts: [
    ...prepared.attempts!, { request_id: "operator", outcome: "unknown", actor: "operator", turn: 0 },
  ] }), null);
  assert.equal(analysisChatPreparedInvestigation({ ...prepared, active: {
    request_id: "active", phase: "investigating", updated_at: empty.updated_at,
  } }), null);
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
    assert.equal(analysisChatDefaultComposerMode({ ...empty, attempts: [{ request_id: "attempt", outcome, turn: 1, actor: "operator" }] }), "question", outcome);
  }
  assert.equal(analysisChatDefaultComposerMode({ ...empty, active: {
    request_id: "active", phase: "investigating", updated_at: empty.updated_at,
  } }), "question");
});

test("the prepared ready view offers explicit next actions without starting another investigation", () => {
  const calls = { fix: 0, differentOutcome: 0 };
  const html = renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme },
    createElement(PreparedInvestigationReady, {
      message: { role: "assistant", prepared: true, request_id: "prepared", content: "Prepared result", created_at: "2026-01-01T00:00:00Z" },
      fixEnabled: true, sourceUnavailable: false,
      onUseForFix: () => { calls.fix++; },
      onDifferentOutcome: () => { calls.differentOutcome++; },
    })));
  assert.match(html, /Prepared investigation is ready/);
  assert.match(html, /Use this finding in a fix proposal/);
  assert.match(html, /<button[^>]*type="button"[^>]*>Investigate a different outcome<\/button>/);
  assert.doesNotMatch(html, /<textarea|role="textbox"|Start investigation|Desired outcome/);
  assert.deepEqual(calls, { fix: 0, differentOutcome: 0 });
});

test("prepared ready Fix actions retain capability, response, and source gates", () => {
  const message: AnalysisChatMessage = {
    role: "assistant", prepared: true, request_id: "prepared", content: "Prepared result", created_at: "2026-01-01T00:00:00Z",
  };
  for (const entry of [
    { name: "disabled capability", message, fixEnabled: false, sourceUnavailable: false },
    { name: "missing request", message: { ...message, request_id: undefined }, fixEnabled: true, sourceUnavailable: false },
    { name: "blank response", message: { ...message, content: " \n\t" }, fixEnabled: true, sourceUnavailable: false },
    { name: "missing source", message, fixEnabled: true, sourceUnavailable: true },
  ]) {
    const html = renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme },
      createElement(PreparedInvestigationReady, {
        message: entry.message, fixEnabled: entry.fixEnabled, sourceUnavailable: entry.sourceUnavailable,
        onUseForFix: () => { assert.fail("rendering must not request a Fix"); },
        onDifferentOutcome: () => { assert.fail("rendering must not start another investigation"); },
      })));
    assert.doesNotMatch(html, /Use this finding in a fix proposal/, entry.name);
    assert.match(html, /Investigate a different outcome/, entry.name);
    if (entry.sourceUnavailable) {
      assert.match(html, /A Fix proposal is unavailable because Aster could not determine the exact source commit this build tested\./);
    } else {
      assert.doesNotMatch(html, /A Fix proposal is unavailable/, entry.name);
    }
  }
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
