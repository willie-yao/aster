import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

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

test("the shared failure escalation request identifies its subject by path", () => {
  const client = source("src/lib/sharedFailureEscalation.ts");

  assert.match(client, /api\/shared-failures\/\$\{encodeURIComponent\(id\)\}\/escalation/);
  // The subject is entirely in the path, so the body carries nothing that
  // could disagree with it.
  assert.match(client, /idempotencyKey, "\{\}"/);
});

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

test("the shared failure route is matched before the pull request number", () => {
  const app = source("src/App.tsx");

  // "shared" would otherwise be captured as a pull request number.
  assert.ok(
    app.indexOf('path="pull-requests/shared/:id"') <
      app.indexOf('path="pull-requests/:number"'),
  );
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
