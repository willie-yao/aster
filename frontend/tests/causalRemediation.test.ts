import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  getCausalRemediationStatus,
  previewCausalFix,
  startCausalRemediation,
  type CausalRemediationRef,
} from "../src/lib/causalRemediation.js";
import type { PatternRemediationInvestigationSummary } from "../src/types/dashboard.js";

const originalFetch = globalThis.fetch;
const ref: CausalRemediationRef = {
  jobID: "periodic/test",
  patternID: "pattern",
  patternHash: "pattern-hash",
  causalGroupID: "group",
  causalGroupHash: "group-hash",
};
const view: PatternRemediationInvestigationSummary = {
  causal_group_id: "group",
  causal_group_hash: "group-hash",
  state: "queued",
};

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("causal remediation start is authenticated, idempotent, and hash-bound", async () => {
  let requestURL = "";
  let requestInit: RequestInit | undefined;
  globalThis.fetch = async (input, init) => {
    requestURL = String(input);
    requestInit = init;
    return new Response(JSON.stringify(view), { status: 202, headers: { "Content-Type": "application/json" } });
  };

  assert.deepEqual(await startCausalRemediation(ref, "request-one", true), view);
  assert.equal(requestURL, "/api/jobs/periodic%2Ftest/patterns/pattern/causal-groups/group/remediation-investigation");
  assert.equal(requestInit?.method, "POST");
  assert.equal(requestInit?.credentials, "same-origin");
  assert.equal(new Headers(requestInit?.headers).get("Idempotency-Key"), "request-one");
  assert.deepEqual(JSON.parse(String(requestInit?.body)), {
    pattern_hash: "pattern-hash",
    causal_group_hash: "group-hash",
    refresh: true,
  });
});

test("causal remediation status requires exact displayed hashes", async () => {
  let requestURL = "";
  globalThis.fetch = async (input) => {
    requestURL = String(input);
    return new Response(JSON.stringify({ ...view, state: "verifying" }), { status: 200 });
  };

  const result = await getCausalRemediationStatus(ref);
  assert.equal(result.state, "verifying");
  assert.match(requestURL, /pattern_hash=pattern-hash/);
  assert.match(requestURL, /causal_group_hash=group-hash/);
});

test("causal remediation errors expose only server-safe response text", async () => {
  globalThis.fetch = async () => new Response("the displayed recurring cause is stale\n", { status: 409 });
  await assert.rejects(() => startCausalRemediation(ref, "request", false), /displayed recurring cause is stale/);
});


test("causal fix preview is hash-bound and has no confirmation field", async () => {
  let requestURL = ""; let requestInit: RequestInit | undefined;
  const preview = { summary: "safe", base_revision: "a".repeat(40), changed_files: ["controller.go"], diff: "diff", validations: [] };
  globalThis.fetch = async (input, init) => { requestURL = String(input); requestInit = init; return new Response(JSON.stringify(preview), { status: 200 }); };
  assert.deepEqual(await previewCausalFix(ref, "preview-one"), preview);
  assert.match(requestURL, /remediation-investigation\/fix-preview$/);
  assert.equal(new Headers(requestInit?.headers).get("Idempotency-Key"), "preview-one");
  assert.deepEqual(JSON.parse(String(requestInit?.body)), { pattern_hash: "pattern-hash", causal_group_hash: "group-hash" });
  assert.equal("token" in preview, false);
});
