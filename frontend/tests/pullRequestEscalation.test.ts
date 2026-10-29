import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { withSSR } from "./helpers/ssr.js";
import type { EscalationView } from "../src/lib/escalation.js";

import {
  escalationActive, getEscalation, startEscalation,
  type EscalationRef, type PullRequestEscalationView,
} from "../src/lib/pullRequestEscalation.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

const { EscalationResult } = await withSSR(async (vite) => {
  return await vite.ssrLoadModule("/src/components/EscalationPanel.tsx") as {
    EscalationResult: (props: { view: EscalationView; disclaimer: string }) => ReturnType<typeof createElement>;
  };
});

for (const disposition of ["preliminary", "citations_verified"] as const) {
  test(`escalation renders ${disposition} quality and warnings honestly`, () => {
    const view: EscalationView = {
      state: "complete", root_cause: "Endpoint mismatch", disposition,
      disposition_warnings: ["investigation_incomplete"], retryable: true, preliminary_attempts: 1,
      citations: [{ path: "build-log.txt", quote: "connection refused", line_start: 7 }],
    };
    const html = renderToStaticMarkup(createElement(EscalationResult, { view, disclaimer: "Not proof of PR causality." }));
    assert.match(html, /Endpoint mismatch/);
    assert.match(html, /investigation_incomplete/);
    assert.match(html, /build-log\.txt/);
    assert.match(html, /Not proof of PR causality/);
    if (disposition === "preliminary") {
      assert.match(html, /Preliminary analysis/);
      assert.match(html, /Treat it as a hypothesis/);
      assert.doesNotMatch(html, /citations verified/);
    } else {
      assert.doesNotMatch(html, /Preliminary analysis/);
    }
  });
}

test("spent preliminary results explain why no quality retry remains", () => {
  const html = renderToStaticMarkup(createElement(EscalationResult, {
    view: { state: "complete", disposition: "preliminary", preliminary_attempts: 3, retryable: false },
    disclaimer: "Evidence only.",
  }));
  assert.match(html, /three-attempt preliminary analysis limit/);
});

test("preliminary retry controls use server eligibility and remain explicit", () => {
  const panel = source("src/components/EscalationPanel.tsx");
  assert.match(panel, /preliminary && view\?\.retryable/);
  assert.match(panel, /Preliminary analysis complete/);
  assert.match(panel, /Analysis complete \(citations verified\)/);
  assert.match(panel, /onClick=\{\(\) => void onStart\(\)\}/);
  assert.equal(panel.match(/calls\.current\.start\(key\)/g)?.length, 1);
});

test("only in-progress escalation states are polled", () => {
  assert.equal(escalationActive("queued"), true);
  assert.equal(escalationActive("running"), true);
  assert.equal(escalationActive("complete"), false);
  assert.equal(escalationActive("failed"), false);
  assert.equal(escalationActive("not_started"), false);
  assert.equal(escalationActive(undefined), false);
});

const ref: EscalationRef = {
  pullNumber: 37,
  jobID: "org/repo job?#",
  buildID: "build/42 +#",
  testName: "[It] creates / cluster? x=y & z+#",
};
const endpoint = "/api/pull-requests/37/checks/org%2Frepo%20job%3F%23/builds/build%2F42%20%2B%23/escalation";

for (const method of ["GET", "POST"] as const) {
  const invoke = () => method === "GET" ? getEscalation(ref) : startEscalation(ref, "pr-attempt-37");

  test(`pull request escalation ${method} sends its exact subject and returns the result`, async (t) => {
    const originalFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = originalFetch; });
    const result: PullRequestEscalationView = {
      ref: { pull_number: 37, job_id: ref.jobID, build_id: ref.buildID, test_name: ref.testName },
      state: "complete",
      root_cause: "The bootstrap image is unavailable.",
      severity: "high",
      suggested_fix: "Publish the image.",
      citations: [{ path: "build-log.txt", line_start: 17, quote: "image missing" }],
      disposition: "preliminary",
      disposition_warnings: ["artifact_grounding_incomplete"],
      preliminary_attempts: 2,
      retryable: true,
    };
    const calls: Array<{ url: unknown; init: RequestInit | undefined }> = [];
    globalThis.fetch = async (url, init) => {
      calls.push({ url, init });
      return Response.json(result);
    };

    assert.deepEqual(await invoke(), result);
    assert.equal(calls.length, 1);
    const { url: input, init } = calls[0];
    const url = new URL(String(input), "https://dashboard.example");
    assert.equal(url.origin, "https://dashboard.example");
    assert.equal(url.pathname, endpoint);
    assert.equal(url.hash, "");
    assert.equal(init?.method ?? "GET", method);
    assert.equal(init?.credentials, "same-origin");
    if (method === "GET") {
      assert.deepEqual([...url.searchParams], [["test", ref.testName]]);
      assert.equal(init?.body, undefined);
    } else {
      assert.equal(url.search, "");
      const headers = new Headers(init?.headers);
      assert.equal(headers.get("Content-Type"), "application/json");
      assert.equal(headers.get("Idempotency-Key"), "pr-attempt-37");
      assert.equal(init?.body, JSON.stringify({ test_name: ref.testName }));
    }
  });

  for (const [body, message] of [
    ["  request rejected\n", "request rejected"],
    [" \n ", "Escalation request failed with HTTP 503."],
  ]) {
    test(`pull request escalation ${method} propagates ${body.trim() ? "server text" : "the HTTP fallback"}`, async (t) => {
      const originalFetch = globalThis.fetch;
      t.after(() => { globalThis.fetch = originalFetch; });
      globalThis.fetch = async () => new Response(body, { status: 503 });
      await assert.rejects(invoke, { message });
    });
  }
}

test("the escalation control is gated on the advertised capability", () => {
  const page = source("src/pages/PullRequestDetailPage.tsx");
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(page, /features\.pull_request_escalation \?\? false/);
  // The panel renders nothing at all when the deploy cannot serve it.
  assert.match(panel, /if \(!enabled\) return null;/);
});

test("escalation reads and controls require an authenticated operator", () => {
  const panel = source("src/components/EscalationPanel.tsx");
  const authProvider = source("src/components/AuthProvider.tsx");

  assert.match(authProvider, /Boolean\(features\.pull_request_escalation\)/);
  assert.match(authProvider, /Boolean\(features\.shared_failure_escalation\)/);
  assert.match(panel, /if \(!enabled \|\| !authenticated\) \{/);
  assert.match(panel, /if \(!authenticated \|\| startingRef\.current\) return;/);
});

test("anonymous escalation panels offer sign-in without protected reads", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /auth\.status === "loading"/);
  assert.match(panel, /Checking operator sign-in/);
  assert.match(panel, /auth\.status === "anonymous"/);
  assert.match(panel, /onClick=\{auth\.signIn\}/);
  assert.match(panel, /Sign in to view or start deeper analysis/);
});

test("sign-out invalidates in-flight escalation state", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /\+\+generation\.current;/);
  assert.match(panel, /requestKey\.current = null;/);
  assert.match(panel, /requestKey\.current === key && issued === generation\.current/);
  assert.match(panel, /setView\(null\);/);
  assert.match(panel, /setError\(null\);/);
});

test("escalation is offered only for failures the baseline could not explain", () => {
  const page = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(page, /needsInvestigation\(failure\) && \(/);
  // A stale build describes a different revision, so it is never escalated.
  assert.match(page, /escalation && !check\.stale/);
});

test("a superseded escalation response cannot overwrite newer state", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /requestKey = useRef<string \| null>\(null\)/);
  assert.match(panel, /requestKey\.current === key/);
  // Unmounting must stop any in-flight update.
  assert.match(panel, /cancelled\.current = true/);
  assert.match(panel, /key=\{props\.subjectKey\}/);
});

test("a slow poll cannot regress state behind a newer start", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  // Reads and writes share a generation counter, so an older in-flight GET
  // cannot overwrite a newer POST result and re-enable Investigate.
  assert.match(panel, /const generation = useRef\(0\)/);
  assert.match(panel, /const issued = \+\+generation\.current/);
  assert.equal(panel.match(/issued === generation\.current/g)?.length, 5);
});

test("polling stops once the escalation reaches a terminal state", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /if \(!enabled \|\| !authenticated \|\| !escalationActive\(view\?\.state\)\) return;/);
  assert.match(panel, /return \(\) => clearInterval\(timer\)/);
});

test("the escalation result does not claim the pull request caused the failure", () => {
  // The panel is subject-agnostic, so each caller supplies the disclaimer that
  // is true for its subject and the panel always renders one.
  const panel = source("src/components/EscalationPanel.tsx");
  const page = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(panel, /\{disclaimer\}/);
  assert.match(page, /does not establish that the pull request caused it/);
});
