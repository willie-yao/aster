import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { escalationActive } from "../src/lib/pullRequestEscalation.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

test("only in-progress escalation states are polled", () => {
  assert.equal(escalationActive("queued"), true);
  assert.equal(escalationActive("running"), true);
  assert.equal(escalationActive("complete"), false);
  assert.equal(escalationActive("failed"), false);
  assert.equal(escalationActive("not_started"), false);
  assert.equal(escalationActive(undefined), false);
});

test("escalation requests are same-origin and carry an idempotency key", () => {
  const client = source("src/lib/pullRequestEscalation.ts");

  assert.match(client, /credentials: "same-origin"/);
  assert.match(client, /"Idempotency-Key": idempotencyKey/);
  // Every path segment is encoded; a Ginkgo test name goes in the body/query.
  assert.match(client, /encodeURIComponent\(ref\.jobID\)/);
  assert.match(client, /encodeURIComponent\(ref\.buildID\)/);
  assert.match(client, /body: JSON\.stringify\(\{ test_name: ref\.testName \}\)/);
});

test("the escalation control is gated on the advertised capability", () => {
  const page = source("src/pages/PullRequestDetailPage.tsx");
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(page, /features\.pull_request_escalation \?\? false/);
  // The panel renders nothing at all when the deploy cannot serve it.
  assert.match(panel, /if \(!enabled\) return null;/);
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
});

test("polling stops once the escalation reaches a terminal state", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /if \(!enabled \|\| !escalationActive\(view\?\.state\)\) return;/);
  assert.match(panel, /return \(\) => clearInterval\(timer\)/);
});

test("the escalation result does not claim the pull request caused the failure", () => {
  const panel = source("src/components/EscalationPanel.tsx");

  assert.match(panel, /does not\s*\n?\s*establish that the pull request caused it/);
});
