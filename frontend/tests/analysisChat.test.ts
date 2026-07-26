import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  analysisChatProgressTurnUsage,
  analysisChatTurnUsage,
  findAnalysisChatSession,
  resumeAnalysisChatTurn,
} from "../src/lib/analysisChat.js";
import type { AnalysisChatReference, AnalysisChatSession } from "../src/types/analysisChat.js";

const originalFetch = globalThis.fetch;

const analysis: AnalysisChatReference = {
  scope: "test",
  job_id: "periodic-demo",
  build_id: "123",
  test_name: "TestCluster",
  suite_name: "suite",
  class_name: "class",
  junit_file: "junit.xml",
  analysis_generated_at: "2026-07-26T12:00:00Z",
};

const session: AnalysisChatSession = {
  id: "session-1",
  analysis,
  created_at: "2026-07-26T12:01:00Z",
  updated_at: "2026-07-26T12:02:00Z",
  expires_at: "2026-07-26T14:01:00Z",
  turns_used: 2,
  max_turns: 10,
  messages: [
    { role: "user", content: "What proves this?", created_at: "2026-07-26T12:01:30Z" },
    { role: "assistant", content: "The log does.", created_at: "2026-07-26T12:02:00Z" },
  ],
};

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("reload and remount restore the latest server session", async () => {
  const requests: Array<{ url: string; init?: RequestInit }> = [];
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), init });
    return new Response(JSON.stringify(session), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  const afterReload = await findAnalysisChatSession(analysis);
  const afterRemount = await findAnalysisChatSession(analysis);

  assert.equal(afterReload?.id, session.id);
  assert.equal(afterRemount?.messages.length, 2);
  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.url, "/api/analysis-chat/sessions/lookup");
    assert.equal(request.init?.method, "POST");
    assert.equal(request.init?.credentials, "same-origin");
    assert.equal(request.init?.cache, "no-store");
    assert.deepEqual(JSON.parse(String(request.init?.body)), analysis);
  }
});

test("missing or expired server sessions restore as empty", async () => {
  globalThis.fetch = async () => new Response(null, { status: 204 });

  assert.equal(await findAnalysisChatSession(analysis), null);
});

test("turn usage comes only from authoritative session fields", () => {
  assert.deepEqual(analysisChatTurnUsage(session), { used: 2, max: 10 });
  assert.deepEqual(analysisChatTurnUsage({ ...session, turns_used: 10 }), { used: 10, max: 10 });
  assert.equal(analysisChatTurnUsage({ ...session, turns_used: Number.NaN }), null);
  assert.equal(analysisChatTurnUsage({ ...session, max_turns: 0 }), null);
  assert.equal(analysisChatTurnUsage({ ...session, turns_used: undefined } as unknown as AnalysisChatSession), null);
  assert.deepEqual(analysisChatProgressTurnUsage({
    request_id: "request", phase: "queued", updated_at: "2026-07-26T12:03:00Z",
    turns_used: 3, max_turns: 10,
  }), { used: 3, max: 10 });
  assert.equal(analysisChatProgressTurnUsage({
    request_id: "request", phase: "queued", updated_at: "2026-07-26T12:03:00Z",
  }), null);
});

test("reload during a turn reconnects the persisted request", async () => {
  const active: AnalysisChatSession = {
    ...session,
    messages: [],
    active: {
      request_id: "request-active",
      question: "What is still running?",
      phase: "reading_evidence",
      updated_at: "2026-07-26T12:03:00Z",
    },
  };
  const completed: AnalysisChatSession = {
    ...session,
    messages: [
      { role: "user", request_id: "request-active", content: "What is still running?", created_at: "2026-07-26T12:04:00Z" },
      { role: "assistant", request_id: "request-active", content: "The request completed.", created_at: "2026-07-26T12:04:00Z" },
    ],
  };
  const progress = {
    request_id: "request-active",
    phase: "finalizing",
    updated_at: "2026-07-26T12:03:30Z",
    turns_used: 3,
    max_turns: 10,
  };
  const events = `event: progress\ndata: ${JSON.stringify(progress)}\n\nevent: session\ndata: ${JSON.stringify(completed)}\n\n`;
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), "/api/analysis-chat/sessions/session-1/messages/stream");
    assert.equal(init?.method, "POST");
    assert.equal(new Headers(init?.headers).get("Idempotency-Key"), "request-active");
    assert.deepEqual(JSON.parse(String(init?.body)), { message: "What is still running?" });
    return new Response(events, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  };
  const phases: string[] = [];

  const restored = await resumeAnalysisChatTurn(active, (value) => phases.push(value.phase));

  assert.equal(restored.active, undefined);
  assert.equal(restored.messages[1]?.request_id, "request-active");
  assert.deepEqual(phases, ["reading_evidence", "finalizing"]);
});

test("version two active sessions poll without a recoverable question", async () => {
  const active: AnalysisChatSession = {
    ...session,
    messages: [],
    active: {
      request_id: "request-legacy",
      phase: "investigating",
      updated_at: "2026-07-26T12:03:00Z",
    },
  };
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), "/api/analysis-chat/sessions/session-1");
    assert.equal(init?.method, undefined);
    return new Response(JSON.stringify(session), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  const restored = await resumeAnalysisChatTurn(active, () => {}, undefined, 0);

  assert.equal(restored.id, session.id);
  assert.equal(restored.active, undefined);
});
