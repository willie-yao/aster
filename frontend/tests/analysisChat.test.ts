import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, test } from "node:test";

import {
  analysisChatAttemptStatus,
  analysisChatFailureGuidance,
  analysisChatHistory,
  analysisChatMarker,
  analysisChatProviderFailureMessage,
  analysisChatRequestState,
  analysisChatResponseValidationMessage,
  analysisChatUnusableAnswerMessage,
  applyPreparedFindingResolution,
  AnalysisChatAPIError,
  analysisChatProgressTurnUsage,
  analysisChatTurnLimitReached,
  analysisChatTurnUsage,
  clearAnalysisChatPendingIntent,
  deleteAnalysisChatSession,
  findAnalysisChatSession,
  isAnalysisChatOAuthExpired,
  loadAnalysisChatPendingIntent,
  lookupPreparedAnalysisChatFindings,
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

const firstCause: AnalysisChatReference = {
  scope: "cause",
  job_id: "periodic-demo",
  pattern_id: "pattern-1",
  pattern_hash: "pattern-hash",
  causal_group_id: "cause-1",
  causal_group_hash: "cause-hash-1",
};

const secondCause: AnalysisChatReference = {
  ...firstCause,
  causal_group_id: "cause-2",
  causal_group_hash: "cause-hash-2",
};

const session: AnalysisChatSession = {
  id: "session-1",
  created_by: "alice",
  analysis,
  created_at: "2026-07-26T12:01:00Z",
  updated_at: "2026-07-26T12:02:00Z",
  expires_at: "2026-07-26T14:01:00Z",
  turns_used: 2,
  max_turns: 10,
  messages: [
    { role: "user", actor: "alice", content: "What proves this?", created_at: "2026-07-26T12:01:30Z" },
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


test("the prepared-finding lookup answers a whole page of causes in one read-only request", async () => {
  const requests: Array<{ url: string; init?: RequestInit }> = [];
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), init });
    return new Response(JSON.stringify({ prepared: [false, true] }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };

  const prepared = await lookupPreparedAnalysisChatFindings([firstCause, secondCause]);

  assert.deepEqual(prepared, [false, true]);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "/api/analysis-chat/prepared/lookup");
  assert.equal(requests[0].init?.method, "POST");
  assert.equal(requests[0].init?.credentials, "same-origin");
  assert.equal(requests[0].init?.cache, "no-store");
  assert.deepEqual(JSON.parse(String(requests[0].init?.body)), { refs: [firstCause, secondCause] });
});

test("an empty batch asks the server nothing", async () => {
  let called = false;
  globalThis.fetch = async () => {
    called = true;
    return new Response(null, { status: 200 });
  };

  assert.deepEqual(await lookupPreparedAnalysisChatFindings([]), []);
  assert.equal(called, false);
});

test("a short or malformed prepared answer marks the missing causes as not ready", async () => {
  globalThis.fetch = async () =>
    new Response(JSON.stringify({ prepared: [true] }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });

  assert.deepEqual(await lookupPreparedAnalysisChatFindings([firstCause, secondCause]), [true, false]);

  globalThis.fetch = async () =>
    new Response(JSON.stringify({}), { status: 200, headers: { "Content-Type": "application/json" } });

  assert.deepEqual(await lookupPreparedAnalysisChatFindings([firstCause]), [false]);
});

test("a failed prepared lookup surfaces as an error rather than a false answer", async () => {
  globalThis.fetch = async () => new Response("nope", { status: 500 });

  await assert.rejects(() => lookupPreparedAnalysisChatFindings([firstCause]), AnalysisChatAPIError);
});

test("a shared conversation supersedes the prepared marker on the collapsed control", () => {
  const base = {
    authenticated: true,
    expanded: false,
    session: null,
    preparedFinding: true,
    restoring: false,
  };

  assert.deepEqual(analysisChatMarker(base), {
    kind: "prepared",
    label: "Finding ready",
    detail:
      "The engine prepared a first answer for this cause. It may challenge the published root cause rather than confirm it.",
  });

  // Opening a prepared cause is what creates the session, and the finding
  // becomes its first message, so the weaker signal must not linger.
  const investigated = analysisChatMarker({ ...base, session });
  assert.equal(investigated?.kind, "investigated");
  assert.equal(investigated?.label, "Investigated by alice");

  const anonymousSession = analysisChatMarker({ ...base, session: { ...session, created_by: "  " } });
  assert.equal(anonymousSession?.label, "Investigated");
});

test("a cause with nothing waiting and a visitor who cannot act get no marker", () => {
  const base = {
    authenticated: true,
    expanded: false,
    session: null,
    preparedFinding: false,
    restoring: false,
  };

  assert.equal(analysisChatMarker(base), null);
  assert.equal(analysisChatMarker({ ...base, preparedFinding: true, authenticated: false }), null);
  // A lookup still in flight has not established anything yet.
  assert.equal(analysisChatMarker({ ...base, preparedFinding: true, restoring: true }), null);
});

test("what the server actually found outlives the cause card that asked", () => {
  // The batch answer is taken at page load and a cause card unmounts its chat
  // when it folds, so a create that found nothing has to be recorded where a
  // fold cannot forget it.
  const loaded = { key: "batch-1", causes: { "cause-1": true, "cause-2": true } };

  const missed = applyPreparedFindingResolution(loaded, "batch-1", "cause-1", false);
  assert.deepEqual(missed.causes, { "cause-1": false, "cause-2": true });

  // A later hit has to clear that correction, or a cause whose finding arrived
  // on the retry would stay silent for the rest of the page's life.
  const recovered = applyPreparedFindingResolution(missed, "batch-1", "cause-1", true);
  assert.deepEqual(recovered.causes, { "cause-1": true, "cause-2": true });

  // Unchanged answers and causes with no chat reference change nothing, so a
  // repeated report cannot churn the page.
  assert.equal(applyPreparedFindingResolution(loaded, "batch-1", "cause-2", true), loaded);
  assert.equal(applyPreparedFindingResolution(loaded, "batch-1", "", false), loaded);
});

test("a correction for causes no longer on the page is dropped", () => {
  const loaded = { key: "batch-1", causes: { "cause-1": true } };

  // The pattern's causes changed while a chat panel was open, so the answer
  // this correction describes is about a cause the page no longer renders.
  assert.equal(applyPreparedFindingResolution(loaded, "batch-2", "cause-1", false), loaded);
});

test("an open panel speaks for itself instead of carrying a marker", () => {
  // The marker exists to say what is waiting before an operator expands the
  // control. Once open, the transcript is authoritative, so a marker could only
  // contradict it: an emptied conversation under a header claiming a finding.
  const expanded = { authenticated: true, expanded: true, session: null, restoring: false };

  assert.equal(analysisChatMarker({ ...expanded, preparedFinding: true }), null);
  assert.equal(analysisChatMarker({ ...expanded, preparedFinding: false, session }), null);
});

test("shared observer refresh cannot overwrite a conversation reset", () => {
  const chat = readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8");
  assert.match(chat, /sessionGenerationRef\.current \+= 1;[\s\S]*await deleteAnalysisChatSession/);
  assert.match(chat, /generation !== sessionGenerationRef\.current/);
  assert.match(chat, /identityRef\.current !== refreshIdentity/);
  assert.match(chat, /busy \|\| restoring \|\| resetting/);
});

test("missing or expired server sessions restore as empty", async () => {
  globalThis.fetch = async () => new Response(null, { status: 204 });

  assert.equal(await findAnalysisChatSession(analysis), null);
});

test("removing a shared conversation tolerates one already gone", async () => {
  const requests: Array<{ url: string; init?: RequestInit }> = [];
  let status = 204;
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), init });
    return new Response(null, { status });
  };

  await deleteAnalysisChatSession("session-1");
  status = 404;
  await deleteAnalysisChatSession("session-1");

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.url, "/api/analysis-chat/sessions/session-1");
    assert.equal(request.init?.method, "DELETE");
    assert.equal(request.init?.credentials, "same-origin");
    assert.equal(request.init?.cache, "no-store");
  }
});

test("a failed discard surfaces the API error instead of clearing the conversation", async () => {
  globalThis.fetch = async () => new Response("analysis chat could not complete the request", { status: 502 });

  await assert.rejects(
    () => deleteAnalysisChatSession("session-1"),
    (error: unknown) => error instanceof AnalysisChatAPIError && error.status === 502,
  );
});

test("synchronous messages serialize the message and nothing else", async () => {
  const bodies: unknown[] = [];
  globalThis.fetch = async (_input, init) => {
    assert.equal(init?.credentials, "same-origin");
    bodies.push(JSON.parse(String(init?.body)));
    return new Response(JSON.stringify(session), { status: 200, headers: { "Content-Type": "application/json" } });
  };

  await sendAnalysisChatMessage("session-1", "normal", "request-normal");
  await sendAnalysisChatMessage("session-1", "second", "request-second");

  // One kind of turn, so the body carries the message and nothing else.
  assert.deepEqual(bodies, [{ message: "normal" }, { message: "second" }]);
});

test("streams serialize the message and reuse one request identity", async () => {
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
  await streamAnalysisChatMessage("session-1", "second", "request-second", () => {});

  assert.deepEqual(bodies, [{ message: "normal" }, { message: "second" }]);
  assert.deepEqual(ids, ["request-normal", "request-second"]);
});

test("ambiguous stream recovery retries without changing the request ID", async () => {
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

  await streamAnalysisChatMessage("session-1", "ask", "request-ask", () => {});

  assert.equal(requests.length, 2);
  assert.deepEqual(requests, [
    { id: "request-ask", body: { message: "ask" } },
    { id: "request-ask", body: { message: "ask" } },
  ]);
});

test("a rejected turn is not retried", async () => {
  let calls = 0;
  globalThis.fetch = async (_input, init) => {
    calls++;
    assert.deepEqual(JSON.parse(String(init?.body)), { message: "ask" });
    return new Response("invalid analysis chat request", {
      status: 400,
      headers: { "X-Analysis-Chat-Outcome": "rejected" },
    });
  };

  await assert.rejects(
    streamAnalysisChatMessage("session-1", "ask", "request-ask", () => {}),
    (error: unknown) => error instanceof AnalysisChatAPIError && error.status === 400,
  );
  assert.equal(calls, 1);
});

test("pending-turn storage retains only request identity and recording state", () => {
  const values = new Map<string, string>();
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
  saveAnalysisChatPendingIntent(storage, {
    analysisIdentity: "analysis", sessionID: "session-1", requestID: "request-fix", requestRecorded: true,
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
  assert.equal(history[0]?.kind === "message" ? history[0].message.actor : undefined, "alice");
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

  const restored = await resumeAnalysisChatTurn(active, (value) => phases.push(value.phase), { requestRecorded: false });

  assert.equal(restored.active, undefined);
  assert.equal(restored.messages[1]?.request_id, "request-active");
  assert.deepEqual(phases, ["reading_evidence", "finalizing"]);
});

test("reload during a recorded turn reconnects with the same request identity", async () => {
  const active: AnalysisChatSession = {
    ...session, messages: [], active: {
      request_id: "request-fix", question: "What should change?", phase: "reading_evidence", updated_at: "2026-07-26T12:03:00Z",
    },
  };
  const events = `event: session\ndata: ${JSON.stringify({ ...session, active: undefined })}\n\n`;
  globalThis.fetch = async (_input, init) => {
    assert.deepEqual(JSON.parse(String(init?.body)), { message: "What should change?" });
    assert.equal(new Headers(init?.headers).get("Idempotency-Key"), "request-fix");
    return new Response(events, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  };

  await resumeAnalysisChatTurn(active, () => {}, { requestRecorded: true });
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
      assert.deepEqual(JSON.parse(String(init?.body)), { message: "What should change?" });
      return new Response("analysis chat source revision changed", { status: 400 });
    }
    if (calls.length === 2) {
      return new Response(JSON.stringify(active), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return new Response(JSON.stringify(completed), { status: 200, headers: { "Content-Type": "application/json" } });
  };

  await assert.rejects(
    resumeAnalysisChatTurn(active, () => {}, { requestRecorded: true }),
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
    resumeAnalysisChatTurn(active, () => {}, { requestRecorded: true }),
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
