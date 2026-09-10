import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import * as ts from "typescript";
import {
  getSharedFailureEscalation, startSharedFailureEscalation,
  type SharedFailureEscalationView,
} from "../src/lib/sharedFailureEscalation.js";

import {
  evidenceMember,
  findSharedFailureFor,
  orderSharedFailures,
  sharedFailureAnalyzable,
  sharedFailureBlockedReason,
  sharedFailureScope,
  sharedFailureSubject,
} from "../src/lib/sharedFailures.js";
import type { SharedFailure, SharedFailureMember } from "../src/types/pullRequests.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

function member(
  number: number,
  started: string,
  overrides: Partial<SharedFailureMember> = {},
): SharedFailureMember {
  return {
    number,
    build_id: `b${number}`,
    started,
    finished: started,
    ...overrides,
  };
}

function failure(overrides: Partial<SharedFailure> = {}): SharedFailure {
  return {
    id: "abc123",
    base_ref: "main",
    job_name: "pull-project-e2e",
    job_id: "example/project/pull-project-e2e",
    test_name: "[It] creates a cluster",
    pull_requests: [
      member(1, "2026-05-01T12:00:00Z"),
      member(2, "2026-05-01T13:00:00Z"),
    ],
    oldest_build_started: "2026-05-01T12:00:00Z",
    newest_build_started: "2026-05-01T13:00:00Z",
    escalatable: true,
    ...overrides,
  };
}

test("shared failures lead with the one hitting the most pull requests", () => {
  const narrow = failure({ id: "narrow", pull_requests: [member(1, "2026-05-01T20:00:00Z")] });
  const wide = failure({ id: "wide" });

  const ordered = orderSharedFailures([narrow, wide]);
  assert.deepEqual(
    ordered.map((f) => f.id),
    ["wide", "narrow"],
  );
});

test("shared failures of equal width are ordered newest first, then stably", () => {
  const older = failure({ id: "older", newest_build_started: "2026-05-01T10:00:00Z" });
  const newer = failure({ id: "newer", newest_build_started: "2026-05-01T18:00:00Z" });

  assert.deepEqual(
    orderSharedFailures([older, newer]).map((f) => f.id),
    ["newer", "older"],
  );

  // Identical width and recency must still produce one deterministic order, so
  // a pass that observes nothing new does not reshuffle the view.
  const a = failure({ id: "aaa" });
  const b = failure({ id: "bbb" });
  assert.deepEqual(
    orderSharedFailures([b, a]).map((f) => f.id),
    ["aaa", "bbb"],
  );
});

test("the evidence build is the newest finished build on a current head", () => {
  const chosen = evidenceMember(failure());
  assert.equal(chosen?.number, 2);
});

test("stale and unfinished builds are never chosen as evidence", () => {
  const stale = failure({
    pull_requests: [
      member(1, "2026-05-01T12:00:00Z"),
      member(2, "2026-05-01T13:00:00Z", { stale: true }),
    ],
  });
  assert.equal(evidenceMember(stale)?.number, 1);

  const running = failure({
    pull_requests: [
      member(1, "2026-05-01T12:00:00Z"),
      member(2, "2026-05-01T13:00:00Z", { finished: undefined }),
    ],
  });
  assert.equal(evidenceMember(running)?.number, 1);

  const none = failure({
    pull_requests: [member(1, "2026-05-01T12:00:00Z", { stale: true })],
  });
  assert.equal(evidenceMember(none), undefined);
});

test("an analysis is offered only when the shared view is the remaining path", () => {
  assert.equal(sharedFailureAnalyzable(failure()), true);
  assert.equal(sharedFailureBlockedReason(failure()), null);

  // A member that can be escalated from its own pull request means the cheaper
  // path already exists, so the shared view must not offer a second one.
  const perPull = failure({ escalatable: false });
  assert.equal(sharedFailureAnalyzable(perPull), false);
  assert.match(String(sharedFailureBlockedReason(perPull)), /affected pull requests/);

  // No readable build means there are no artifacts to analyze yet.
  const noEvidence = failure({
    pull_requests: [member(1, "2026-05-01T12:00:00Z", { finished: undefined })],
  });
  assert.equal(sharedFailureAnalyzable(noEvidence), false);
  assert.match(String(sharedFailureBlockedReason(noEvidence)), /finished build/);
});

test("a build-level failure is named by its job, not its generic test name", () => {
  assert.equal(sharedFailureSubject(failure()), "[It] creates a cluster");
  assert.equal(
    sharedFailureSubject(failure({ build_level: true })),
    "pull-project-e2e",
  );
});

test("the scope states how many pull requests and which branch", () => {
  assert.equal(sharedFailureScope(failure()), "2 pull requests targeting main");
  assert.equal(
    sharedFailureScope(failure({ pull_requests: [member(1, "2026-05-01T12:00:00Z")] })),
    "1 pull request targeting main",
  );
});

test("a failure is matched to its cluster on the whole correlation key", () => {
  const clusters = [failure()];

  assert.equal(
    findSharedFailureFor(clusters, "main", "pull-project-e2e", "[It] creates a cluster")?.id,
    "abc123",
  );
  // A different base branch or job is a different failure, so neither matches.
  assert.equal(
    findSharedFailureFor(clusters, "release-1.0", "pull-project-e2e", "[It] creates a cluster"),
    undefined,
  );
  assert.equal(
    findSharedFailureFor(clusters, "main", "other-job", "[It] creates a cluster"),
    undefined,
  );
  assert.equal(findSharedFailureFor(undefined, "main", "j", "t"), undefined);
});

for (const method of ["GET", "POST"] as const) {
  const id = "shared/cluster ?+#";
  const invoke = () => method === "GET"
    ? getSharedFailureEscalation(id)
    : startSharedFailureEscalation(id, "shared-attempt-9");

  test(`shared failure escalation ${method} identifies its subject only by path`, async (t) => {
    const originalFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = originalFetch; });
    const result: SharedFailureEscalationView = {
      ref: { id },
      state: "complete",
      root_cause: "The registry is unavailable.",
      evidence: { repo: "org/repo", pull_number: 9, build_id: "42" },
      citations: [{ path: "build-log.txt", line_start: 8, quote: "registry timeout" }],
    };
    const calls: Array<{ url: unknown; init: RequestInit | undefined }> = [];
    globalThis.fetch = async (url, init) => {
      calls.push({ url, init });
      return Response.json(result);
    };

    assert.deepEqual(await invoke(), result);
    assert.equal(calls.length, 1);
    const { url: input, init } = calls[0];
    assert.equal(String(input), "/api/shared-failures/shared%2Fcluster%20%3F%2B%23/escalation");
    assert.equal(init?.method ?? "GET", method);
    assert.equal(init?.credentials, "same-origin");
    if (method === "GET") {
      assert.equal(init?.body, undefined);
    } else {
      const headers = new Headers(init?.headers);
      assert.equal(headers.get("Content-Type"), "application/json");
      assert.equal(headers.get("Idempotency-Key"), "shared-attempt-9");
      assert.equal(init?.body, "{}");
    }
  });

  for (const [body, message] of [
    ["  shared request rejected\n", "shared request rejected"],
    [" \n ", "Escalation request failed with HTTP 409."],
  ]) {
    test(`shared failure escalation ${method} propagates ${body.trim() ? "server text" : "the HTTP fallback"}`, async (t) => {
      const originalFetch = globalThis.fetch;
      t.after(() => { globalThis.fetch = originalFetch; });
      globalThis.fetch = async () => new Response(body, { status: 409 });
      await assert.rejects(invoke, { message });
    });
  }
}

test("the shared failure control is gated on its own advertised capability", () => {
  const page = source("src/pages/SharedFailurePage.tsx");

  // Riding on pull_request_escalation would advertise a control this server
  // may not serve, since the two services construct independently.
  assert.match(page, /features\.shared_failure_escalation \?\? false/);
  assert.doesNotMatch(page, /features\.pull_request_escalation/);
});

test("a widespread verdict links to the shared failure instead of a peer", () => {
  const detail = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(detail, /findSharedFailureFor\(clusters, baseRef, check\.job_name, failure\.name\)/);
  assert.match(detail, /to=\{sharedFailurePath\(cluster\.id\)\}/);
});

test("App registers the shared failure and pull request detail routes exactly once", () => {
  const file = ts.createSourceFile("App.tsx", source("src/App.tsx"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const app = file.statements.find((node): node is ts.FunctionDeclaration =>
    ts.isFunctionDeclaration(node) && node.name?.text === "App");
  assert.ok(app?.body, "missing App route owner");
  const rendered = app.body.statements.find(ts.isReturnStatement)?.expression;
  assert.ok(rendered, "missing App render");
  const routeTrees: ts.JsxElement[] = [];
  function findRoutes(node: ts.Node) {
    if (ts.isJsxElement(node) && node.openingElement.tagName.getText(file) === "Routes") routeTrees.push(node);
    ts.forEachChild(node, findRoutes);
  }
  findRoutes(rendered);
  assert.equal(routeTrees.length, 1, "App must render its route table");
  const registrations: Array<{ path: string; component: string }> = [];
  function visit(node: ts.Node) {
    if ((ts.isJsxSelfClosingElement(node) || ts.isJsxOpeningElement(node)) && node.tagName.getText(file) === "Route") {
      const attributes = node.attributes.properties.filter(ts.isJsxAttribute);
      const path = attributes.find((attribute) => attribute.name.getText(file) === "path")?.initializer;
      const element = attributes.find((attribute) => attribute.name.getText(file) === "element")?.initializer;
      if (path && ts.isStringLiteral(path)) {
        const child = element && ts.isJsxExpression(element) ? element.expression : undefined;
        const component = child && ts.isJsxSelfClosingElement(child) ? child.tagName.getText(file) : "";
        registrations.push({ path: path.text, component });
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(routeTrees[0]);
  for (const [path, component] of [
    ["pull-requests/shared/:id", "SharedFailurePage"],
    ["pull-requests/:number", "PullRequestDetailPage"],
  ]) {
    assert.deepEqual(registrations.filter((route) => route.path === path), [{ path, component }]);
  }
});

test("the shared failure view says its build window is not a start time", () => {
  const page = source("src/pages/SharedFailurePage.tsx");

  // A pass sees only the newest build per check, so claiming a first-observed
  // time would assert something the data cannot support.
  assert.match(page, /not when the failure started/);
});

test("a completed analysis names the build it actually read", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  // The newest usable build moves between passes while the shared failure id
  // stays put, so the result must state its own evidence rather than let the
  // reader infer it from the current member list.
  assert.match(panel, /view\.evidence\?\.build_id && \(/);
  assert.match(panel, /Read from build \{view\.evidence\.build_id\}/);
});

test("the member list only claims which build would be analyzed", () => {
  const page = source("src/pages/SharedFailurePage.tsx");

  // Before any analysis runs, calling a build "analyzed" would be false.
  assert.doesNotMatch(page, /supplies the artifacts analyzed here/);
  assert.match(page, /newest usable build/);
});

test("the shared failure view does not claim the affected changes are unrelated", () => {
  const page = source("src/pages/SharedFailurePage.tsx");

  // Correlating on base branch, job, and test cannot establish independence:
  // one pull request may be stacked on another.
  assert.doesNotMatch(page, /unrelated changes/);
  assert.match(page, /not established to be independent/);
});
