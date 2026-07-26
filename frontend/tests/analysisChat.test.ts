import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { findAnalysisChatSession } from "../src/lib/analysisChat.js";
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
