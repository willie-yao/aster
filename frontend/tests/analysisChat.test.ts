import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  analysisChatCitationValidationMessage,
  analysisChatAttemptStatus,
  analysisChatFailureGuidance,
  analysisChatHistory,
  analysisChatProviderFailureMessage,
  analysisChatRequestState,
  analysisChatResponseValidationMessage,
  AnalysisChatAPIError,
  analysisChatProgressTurnUsage,
  analysisChatTurnLimitReached,
  analysisChatTurnUsage,
  findAnalysisChatSession,
  markAnalysisChatTurnLimitReached,
  resumeAnalysisChatTurn,
} from "../src/lib/analysisChat.js";
import type {
  AnalysisChatAttempt,
  AnalysisChatReference,
  AnalysisChatSession,
} from "../src/types/analysisChat.js";

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
  assert.equal(markAnalysisChatTurnLimitReached({ ...session, turns_used: 9 }).turns_used, 10);
  assert.equal(markAnalysisChatTurnLimitReached({ ...session, turns_used: 12 }).turns_used, 12);
  assert.equal(analysisChatTurnLimitReached({ ...session, turns_used: 10 }, false, false), true);
  assert.equal(analysisChatTurnLimitReached({ ...session, turns_used: 10 }, true, false), false);
  assert.equal(analysisChatTurnLimitReached(session, false, true), true);
  assert.equal(analysisChatTurnLimitReached(session, true, true), false);
  assert.deepEqual(analysisChatTurnUsage({
    ...session,
    turns_used: 3,
    attempts: Array.from({ length: 8 }, (_, index) => ({
      request_id: `request-${index}`,
      outcome: "failed" as const,
    })),
  }), { used: 3, max: 10 });
});

test("restored attempt history is safe, ordered, and does not duplicate successful messages", () => {
  const attempts: AnalysisChatAttempt[] = [
    { request_id: "success", question: "What proves this?", outcome: "succeeded", turn: 1 },
    { request_id: "cancelled", question: "Stop this", outcome: "cancelled", turn: 2 },
    { request_id: "provider", question: "Provider question", outcome: "failed", failure_kind: "provider", turn: 3 },
    { request_id: "validation", question: "Validation question", outcome: "failed", failure_kind: "validation", turn: 4 },
    { request_id: "citation", question: "Citation question", outcome: "failed", failure_kind: "citation", turn: 5 },
    { request_id: "timeout", question: "Timeout question", outcome: "timed_out", turn: 6 },
    { request_id: "unknown", question: "Unknown question", outcome: "unknown", turn: 7 },
    { request_id: "pending", question: "Pending question", outcome: "pending", turn: 8 },
    { request_id: "success-missing", question: "Recovered success", outcome: "succeeded", turn: 9 },
    { request_id: "legacy-cancelled", outcome: "cancelled" },
  ];
  const history = analysisChatHistory({
    ...session,
    attempts,
    messages: session.messages.map((message) => ({ ...message, request_id: "success" })),
  });

  assert.deepEqual(
    history.map((entry) => entry.kind === "message" ? `message:${entry.message.role}` : entry.attempt.request_id),
    [
      "message:user",
      "message:assistant",
      "cancelled",
      "provider",
      "validation",
      "citation",
      "timeout",
      "unknown",
      "pending",
      "success-missing",
      "legacy-cancelled",
    ],
  );
  assert.equal(history.some((entry) => entry.kind === "attempt" && entry.attempt.request_id === "success"), false);
  assert.deepEqual(
    attempts.slice(1).map((attempt) => analysisChatAttemptStatus(attempt).label),
    [
      "Request cancelled",
      "Provider request failed",
      "Response validation failed",
      "Evidence citation validation failed",
      "Request timed out",
      "Outcome unknown",
      "Request pending",
      "Request completed",
      "Request cancelled",
    ],
  );
  const rendered = attempts.slice(1).map((attempt) => analysisChatAttemptStatus(attempt)).map((status) => `${status.label} ${status.detail}`).join(" ");
  for (const privateValue of ["provider error", "system prompt", "provider token", "/private/path"]) {
    assert.equal(rendered.includes(privateValue), false);
  }
});

test("reconciliation releases terminal attempts and retains only pending requests", () => {
  const base = { ...session, messages: [] };
  for (const outcome of ["failed", "cancelled", "timed_out", "unknown"] as const) {
    assert.equal(analysisChatRequestState({
      ...base,
      attempts: [{ request_id: "request", outcome }],
    }, "request"), "terminal");
  }
  assert.equal(analysisChatRequestState({
    ...base,
    attempts: [{ request_id: "request", outcome: "succeeded" }],
  }, "request"), "succeeded");
  assert.equal(analysisChatRequestState({
    ...base,
    attempts: [{ request_id: "request", outcome: "pending" }],
  }, "request"), "pending");
  assert.equal(analysisChatRequestState({
    ...base,
    active: { request_id: "request", phase: "investigating", updated_at: "2026-07-26T12:03:00Z" },
  }, "request"), "pending");
  assert.equal(analysisChatRequestState({
    ...base,
    messages: [{ role: "assistant", request_id: "request", content: "answer", created_at: "2026-07-26T12:03:00Z" }],
  }, "request"), "answered");
  assert.equal(analysisChatRequestState(base, "request"), "unresolved");
});

test("safe analysis chat failures include recovery guidance", () => {
  const cases = [
    [analysisChatProviderFailureMessage, "Try again in a moment"],
    [analysisChatResponseValidationMessage, "Try a narrower question"],
    [analysisChatCitationValidationMessage, "Try a narrower evidence question"],
  ] as const;
  for (const [message, guidance] of cases) {
    const rendered = analysisChatFailureGuidance(new AnalysisChatAPIError(502, message, "failed"));
    assert.ok(rendered?.includes(guidance), rendered ?? "missing guidance");
  }
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
