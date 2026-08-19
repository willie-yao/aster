import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  analysisChatAttemptStatus,
  analysisChatFailureGuidance,
  analysisChatHistory,
  analysisChatProviderFailureMessage,
  analysisChatRequestState,
  analysisChatResponseValidationMessage,
  analysisChatUnusableAnswerMessage,
  AnalysisChatAPIError,
  analysisChatProgressTurnUsage,
  analysisChatTurnLimitReached,
  analysisChatTurnUsage,
  beginAnalysisChatFixInvestigation,
  clearAnalysisChatPendingIntent,
  findAnalysisChatSession,
  isAnalysisChatOAuthExpired,
  loadAnalysisChatPendingIntent,
  markAnalysisChatTurnLimitReached,
  reconcileAnalysisChatTurn,
  resumeAnalysisChatTurn,
  saveAnalysisChatPendingIntent,
  sendAnalysisChatMessage,
  streamAnalysisChatMessage,
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

test("Fix investigation aborts restore and creates only fresh sessions", async () => {
  const requests: Array<{ url: string; id: string | null }> = [];
  let restoreAborted = false;
  globalThis.fetch = async (input, init) => {
    assert.equal(restoreAborted, true);
    requests.push({
      url: String(input),
      id: new Headers(init?.headers).get("Idempotency-Key"),
    });
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.deepEqual(JSON.parse(String(init?.body)), analysis);
    return new Response(JSON.stringify(session), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  const first = beginAnalysisChatFixInvestigation(analysis, () => { restoreAborted = true; });
  await first.session;
  restoreAborted = false;
  const second = beginAnalysisChatFixInvestigation(analysis, () => { restoreAborted = true; });
  await second.session;

  assert.notEqual(first.requestID, second.requestID);
  assert.deepEqual(requests.map((request) => request.url), [
    "/api/analysis-chat/sessions",
    "/api/analysis-chat/sessions",
  ]);
  assert.deepEqual(requests.map((request) => request.id), [first.requestID, second.requestID]);
  assert.equal(requests.some((request) => /preview|confirm/.test(request.url)), false);
});

test("normal and Fix-intended synchronous messages serialize only the requested intent", async () => {
  const bodies: unknown[] = [];
  globalThis.fetch = async (_input, init) => {
    assert.equal(init?.credentials, "same-origin");
    bodies.push(JSON.parse(String(init?.body)));
    return new Response(JSON.stringify(session), { status: 200, headers: { "Content-Type": "application/json" } });
  };

  await sendAnalysisChatMessage("session-1", "normal", "request-normal");
  await sendAnalysisChatMessage("session-1", "fix", "request-fix", { fixIntent: true });

  assert.deepEqual(bodies, [{ message: "normal" }, { message: "fix", fix_intent: true }]);
});

test("normal and Fix-intended streams serialize intent and reuse one request identity", async () => {
  const completed: AnalysisChatSession = { ...session, active: undefined };
  const events = `event: session\ndata: ${JSON.stringify(completed)}\n\n`;
  const bodies: unknown[] = [];
  const ids: string[] = [];
  globalThis.fetch = async (_input, init) => {
    bodies.push(JSON.parse(String(init?.body)));
    ids.push(new Headers(init?.headers).get("Idempotency-Key") ?? "");
    return new Response(events, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  };

  await streamAnalysisChatMessage("session-1", "normal", "request-normal", () => {});
  await streamAnalysisChatMessage("session-1", "fix", "request-fix", () => {}, { fixIntent: true });

  assert.deepEqual(bodies, [{ message: "normal" }, { message: "fix", fix_intent: true }]);
  assert.deepEqual(ids, ["request-normal", "request-fix"]);
});

test("ambiguous Fix stream recovery preserves intent without changing the request ID", async () => {
  const completed: AnalysisChatSession = { ...session, active: undefined };
  const events = `event: session\ndata: ${JSON.stringify(completed)}\n\n`;
  const requests: Array<{ id: string | null; body: unknown }> = [];
  globalThis.fetch = async (_input, init) => {
    requests.push({
      id: new Headers(init?.headers).get("Idempotency-Key"),
      body: JSON.parse(String(init?.body)),
    });
    if (requests.length === 1) throw new TypeError("connection reset");
    return new Response(events, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  };

  await streamAnalysisChatMessage("session-1", "fix", "request-fix", () => {}, { fixIntent: true });

  assert.equal(requests.length, 2);
  assert.deepEqual(requests, [
    { id: "request-fix", body: { message: "fix", fix_intent: true } },
    { id: "request-fix", body: { message: "fix", fix_intent: true } },
  ]);
});

test("Fix-intent preflight failure is not retried or downgraded to normal chat", async () => {
  let calls = 0;
  globalThis.fetch = async (_input, init) => {
    calls++;
    assert.deepEqual(JSON.parse(String(init?.body)), { message: "fix", fix_intent: true });
    return new Response("invalid analysis chat request", {
      status: 400,
      headers: { "X-Analysis-Chat-Outcome": "rejected" },
    });
  };

  await assert.rejects(
    streamAnalysisChatMessage("session-1", "fix", "request-fix", () => {}, { fixIntent: true }),
    (error: unknown) => error instanceof AnalysisChatAPIError && error.status === 400,
  );
  assert.equal(calls, 1);
});

test("pending-turn intent storage retains only request identity and intent", () => {
  const values = new Map<string, string>();
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
  saveAnalysisChatPendingIntent(storage, {
    analysisIdentity: "analysis", sessionID: "session-1", requestID: "request-fix", fixIntent: true,
  });
  assert.equal(loadAnalysisChatPendingIntent(storage, "analysis", "session-1", "request-fix"), true);
  assert.equal(loadAnalysisChatPendingIntent(storage, "other", "session-1", "request-fix"), undefined);
  assert.doesNotMatch(Array.from(values.values())[0] ?? "", /token|cookie|question/i);
  clearAnalysisChatPendingIntent(storage, "session-1", "request-fix");
  assert.equal(values.size, 0);
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
    [analysisChatResponseValidationMessage, "did not match the answer contract"],
    [analysisChatUnusableAnswerMessage, "did not return an answer"],
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

  const restored = await resumeAnalysisChatTurn(active, (value) => phases.push(value.phase), { fixIntent: false });

  assert.equal(restored.active, undefined);
  assert.equal(restored.messages[1]?.request_id, "request-active");
  assert.deepEqual(phases, ["reading_evidence", "finalizing"]);
});

test("reload during a Fix-intended turn reconnects with Fix intent", async () => {
  const active: AnalysisChatSession = {
    ...session, messages: [], active: {
      request_id: "request-fix", question: "What should change?", phase: "reading_evidence", updated_at: "2026-07-26T12:03:00Z",
    },
  };
  const events = `event: session\ndata: ${JSON.stringify({ ...session, active: undefined })}\n\n`;
  globalThis.fetch = async (_input, init) => {
    assert.deepEqual(JSON.parse(String(init?.body)), { message: "What should change?", fix_intent: true });
    assert.equal(new Headers(init?.headers).get("Idempotency-Key"), "request-fix");
    return new Response(events, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  };

  await resumeAnalysisChatTurn(active, () => {}, { fixIntent: true });
});

test("deterministic reconnect failure observes an admitted request without a second POST", async () => {
  const active: AnalysisChatSession = {
    ...session, messages: [], active: {
      request_id: "request-fix", question: "What should change?", phase: "reading_evidence", updated_at: "2026-07-26T12:03:00Z",
    },
  };
  const completed: AnalysisChatSession = {
    ...session,
    active: undefined,
    messages: [
      { role: "user", content: "What should change?", request_id: "request-fix", created_at: "2026-07-26T12:03:00Z" },
      { role: "assistant", content: "Change `reconcileThing`.", request_id: "request-fix", created_at: "2026-07-26T12:04:00Z" },
    ],
  };
  const calls: string[] = [];
  globalThis.fetch = async (input, init) => {
    const method = init?.method ?? "GET";
    calls.push(`${method} ${String(input)}`);
    if (method === "POST") {
      assert.deepEqual(JSON.parse(String(init?.body)), { message: "What should change?", fix_intent: true });
      return new Response("analysis chat source revision changed", { status: 400 });
    }
    if (calls.length === 2) {
      return new Response(JSON.stringify(active), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return new Response(JSON.stringify(completed), { status: 200, headers: { "Content-Type": "application/json" } });
  };

  await assert.rejects(
    resumeAnalysisChatTurn(active, () => {}, { fixIntent: true }),
    (error: unknown) => error instanceof AnalysisChatAPIError && error.status === 400,
  );
  const reconciled = await reconcileAnalysisChatTurn("session-1", "request-fix", () => {}, { pollDelayMs: 0 });

  assert.equal(reconciled.state, "answered");
  assert.deepEqual(calls, [
    "POST /api/analysis-chat/sessions/session-1/messages/stream",
    "GET /api/analysis-chat/sessions/session-1",
    "GET /api/analysis-chat/sessions/session-1",
  ]);
});

test("OAuth expiry is recognized for lookup and active-turn restoration", async () => {
  globalThis.fetch = async () => new Response("unauthorized", { status: 401 });

  await assert.rejects(
    findAnalysisChatSession(analysis),
    (error: unknown) => isAnalysisChatOAuthExpired(error, "oauth"),
  );
  const active: AnalysisChatSession = {
    ...session, messages: [], active: {
      request_id: "request-fix", question: "What should change?", phase: "reading_evidence", updated_at: "2026-07-26T12:03:00Z",
    },
  };
  await assert.rejects(
    resumeAnalysisChatTurn(active, () => {}, { fixIntent: true }),
    (error: unknown) => isAnalysisChatOAuthExpired(error, "oauth"),
  );
  assert.equal(isAnalysisChatOAuthExpired(new AnalysisChatAPIError(401, "unauthorized"), "dev"), false);
});

test("active turns with unknown intent poll instead of resubmitting without intent", async () => {
  const active: AnalysisChatSession = {
    ...session, messages: [], active: {
      request_id: "request-unknown", question: "What should change?", phase: "reading_evidence", updated_at: "2026-07-26T12:03:00Z",
    },
  };
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), "/api/analysis-chat/sessions/session-1");
    assert.equal(init?.method, undefined);
    return new Response(JSON.stringify({ ...session, active: undefined }), {
      status: 200, headers: { "Content-Type": "application/json" },
    });
  };

  await resumeAnalysisChatTurn(active, () => {}, { pollDelayMs: 0 });
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

  const restored = await resumeAnalysisChatTurn(active, () => {}, { pollDelayMs: 0 });

  assert.equal(restored.id, session.id);
  assert.equal(restored.active, undefined);
});

test("build analysis lookup keeps the source discriminator and omits JUnit identity", async () => {
  const buildAnalysis: AnalysisChatReference = {
    scope: "test", job_id: "periodic-demo", build_id: "123", test_name: "Prow job execution",
    source: "build", suite_name: "Prow", class_name: "job", analysis_generated_at: "2026-07-30T12:00:00Z",
  };
  globalThis.fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body));
    assert.equal(body.source, "build");
    assert.equal("junit_file" in body, false);
    return new Response(null, { status: 204 });
  };
  assert.equal(await findAnalysisChatSession(buildAnalysis), null);
});
