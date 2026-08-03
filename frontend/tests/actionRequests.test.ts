import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  actionRequestCanConfirm,
  actionRequestHasBlockingVerification,
  actionRequestIsActive,
  actionRequestIsPollable,
  actionRequestIsRecoverable,
  actionRequestIsTerminal,
  actionRequestProgressDetail,
  actionRequestProgressTitle,
  actionRequestStorageKey,
  actionRequestStorageOwner,
  actionRequestVerificationDetail,
  actionRequestVerificationTitle,
  cancelActionRequest,
  loadLatestActionRequest,
  readStoredActionRequestID,
  syncStoredActionRequest,
  type ActionRequestStorage,
} from "../src/lib/actionRequests.js";
import type {
  Action,
  ActionRequest,
  RequestStatus,
} from "../src/types/actions.js";

const originalFetch = globalThis.fetch;

class MemoryStorage implements ActionRequestStorage {
  readonly values = new Map<string, string>();

  getItem(key: string): string | null {
    return this.values.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.values.set(key, value);
  }

  removeItem(key: string): void {
    this.values.delete(key);
  }
}

function request(
  id: string,
  status: RequestStatus,
  overrides: Partial<ActionRequest> = {},
): ActionRequest {
  return {
    id,
    failure_id: "pattern-1",
    kind: "propose-fix",
    owner: "Maintainer",
    status,
    created_at: "2026-08-03T12:00:00Z",
    updated_at: "2026-08-03T12:01:00Z",
    expires_at: "2026-08-03T14:00:00Z",
    ...overrides,
  };
}

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("pending and cancelling requests remain active and pollable", () => {
  for (const status of ["pending", "cancelling"] as const) {
    assert.equal(actionRequestIsPollable(status), true);
    assert.equal(actionRequestIsActive(request(`request-${status}`, status), 0), true);
    assert.equal(actionRequestIsRecoverable(status), true);
    assert.equal(actionRequestIsTerminal(status), false);
  }
  assert.equal(actionRequestIsPollable("ready"), false);
  assert.equal(actionRequestIsActive(request("ready", "ready"), 0), true);
  assert.equal(
    actionRequestIsActive(
      request("expired-ready", "ready", { expires_at: "2026-08-03T10:00:00Z" }),
      Date.parse("2026-08-03T11:00:00Z"),
    ),
    false,
  );
  assert.equal(actionRequestIsActive(request("unknown", "unknown"), 0), true);
  assert.equal(actionRequestIsRecoverable("ready"), true);
  assert.equal(actionRequestIsRecoverable("unknown"), true);
});

test("unknown outcomes remain confirmable without a visible preview", () => {
  assert.equal(actionRequestCanConfirm("unknown", false), true);
  assert.equal(actionRequestCanConfirm("ready", false), false);
  assert.equal(actionRequestCanConfirm("ready", true), true);
  assert.equal(actionRequestCanConfirm("cancelling", true), false);
});

test("source verification progress is distinct from draft generation", () => {
  const verifying = request("verifying", "pending", {
    stage: "verifying_remediation",
  });
  assert.equal(
    actionRequestProgressTitle(verifying, false),
    "Verifying the proposed remediation against pinned source",
  );
  assert.match(actionRequestProgressDetail(verifying), /before starting any model/);

  const drafting = request("drafting", "pending", {
    stage: "drafting",
    verification: {
      state: "unresolved",
      reason: "verified target remains unresolved",
    },
  });
  assert.equal(actionRequestProgressTitle(drafting, true), "Generating the fix proposal");
  assert.match(actionRequestProgressDetail(drafting), /verified as unresolved/);
});

test("source verification outcomes have specific labels", () => {
  const existing = request("existing", "failed", {
    verification: {
      state: "already_present",
      reason: "LabelCRDsForClusterctlUpgrade is already defined and used",
    },
  });
  assert.equal(actionRequestVerificationTitle(existing), "Existing remediation detected");
  assert.match(actionRequestVerificationDetail(existing) ?? "", /stale, regressed, or misclassified/);
  assert.equal(actionRequestHasBlockingVerification(existing), true);

  const configuration = request("configuration", "failed", {
    verification: {
      state: "already_present",
      reason: "configuration GenericWorkload=true is already applied",
    },
  });
  assert.equal(actionRequestVerificationTitle(configuration), "Configuration already applied");

  const removedConfiguration = request("removed-configuration", "failed", {
    verification: {
      state: "already_present",
      reason: "configuration LegacyGate=true is already absent from templates/dra.yaml",
    },
  });
  assert.equal(
    actionRequestVerificationTitle(removedConfiguration),
    "Configuration already absent",
  );

  const inconclusive = request("inconclusive", "failed", {
    verification: {
      state: "inconclusive",
      reason: "proposal has no implementation-ready source target",
    },
  });
  assert.equal(actionRequestVerificationTitle(inconclusive), "Source verification inconclusive");
  assert.match(actionRequestVerificationDetail(inconclusive) ?? "", /Investigate the pinned source/);
  assert.equal(actionRequestHasBlockingVerification(inconclusive), true);
});

test("active request storage is isolated by owner failure and action", () => {
  const storage = new MemoryStorage();
  const cases: Array<[string, string, Action, string]> = [
    ["maintainer", "pattern-1", "propose-fix", "fix-request"],
    ["other", "pattern-1", "propose-fix", "other-owner-request"],
    ["maintainer", "pattern-2", "propose-fix", "other-failure-request"],
    ["maintainer", "pattern-1", "create-issue", "issue-request"],
  ];

  for (const [owner, failureID, action, id] of cases) {
    syncStoredActionRequest(
      storage,
      owner,
      request(id, "pending", { failure_id: failureID, kind: action }),
    );
  }

  for (const [owner, failureID, action, id] of cases) {
    assert.equal(
      readStoredActionRequestID(storage, owner, failureID, action),
      id,
    );
  }
  assert.equal(storage.values.size, cases.length);
  assert.notEqual(
    actionRequestStorageKey("maintainer", "pattern-1", "propose-fix"),
    actionRequestStorageKey("other", "pattern-1", "propose-fix"),
  );
});

test("reload and remount recover one stored active request ID", () => {
  const storage = new MemoryStorage();
  const value = request("request-1", "pending");

  syncStoredActionRequest(storage, "maintainer", value);

  assert.equal(
    readStoredActionRequestID(
      storage,
      "maintainer",
      value.failure_id,
      value.kind,
    ),
    value.id,
  );
  assert.equal(
    readStoredActionRequestID(
      storage,
      "maintainer",
      value.failure_id,
      value.kind,
    ),
    value.id,
  );

  syncStoredActionRequest(storage, "maintainer", value);
  assert.equal(storage.values.size, 1);
});

test("storage clears only terminal requests and retains unknown outcomes", () => {
  const storage = new MemoryStorage();
  const retained = ["pending", "cancelling", "ready", "unknown"] as const;
  const terminal = ["failed", "confirmed", "cancelled", "expired"] as const;

  for (const status of retained) {
    const value = request(`request-${status}`, status);
    syncStoredActionRequest(storage, "maintainer", value);
    assert.equal(
      readStoredActionRequestID(
        storage,
        "maintainer",
        value.failure_id,
        value.kind,
      ),
      value.id,
      status,
    );
  }

  for (const status of terminal) {
    const active = request(`active-before-${status}`, "ready");
    syncStoredActionRequest(storage, "maintainer", active);
    syncStoredActionRequest(
      storage,
      "maintainer",
      request(`request-${status}`, status),
    );
    assert.equal(
      readStoredActionRequestID(
        storage,
        "maintainer",
        active.failure_id,
        active.kind,
      ),
      null,
      status,
    );
  }
});

test("storage owner uses the authenticated login when available", () => {
  assert.equal(actionRequestStorageOwner(" Maintainer ", "oauth"), "maintainer");
  assert.equal(actionRequestStorageOwner(null, "proxy"), "mode:proxy");
  assert.equal(actionRequestStorageOwner(null, null), null);
});

test("superseded request chains resolve to the latest request", async () => {
  const calls: string[] = [];
  globalThis.fetch = async (input) => {
    const url = String(input);
    calls.push(url);
    if (url.endsWith("/request-old")) {
      return Response.json(
        request("request-old", "cancelling", {
          superseded_by: "request-next",
        }),
      );
    }
    return Response.json(request("request-next", "ready"));
  };

  const latest = await loadLatestActionRequest("/dashboard/", "request-old");

  assert.equal(latest.id, "request-next");
  assert.deepEqual(calls, [
    "/dashboard/api/action-requests/request-old",
    "/dashboard/api/action-requests/request-next",
  ]);
});

test("replacement cycles are rejected", async () => {
  globalThis.fetch = async (input) => {
    const id = String(input).endsWith("/request-a") ? "request-a" : "request-b";
    return Response.json(
      request(id, "cancelling", {
        superseded_by: id === "request-a" ? "request-b" : "request-a",
      }),
    );
  };

  await assert.rejects(
    loadLatestActionRequest("/", "request-a"),
    /replacement cycle detected/,
  );
});

test("request IDs are URL encoded for reads and cancellation", async () => {
  const calls: Array<{ url: string; init?: RequestInit }> = [];
  globalThis.fetch = async (input, init) => {
    calls.push({ url: String(input), init });
    return Response.json(request("request / one", "cancelling"));
  };

  await loadLatestActionRequest("/", "request / one");
  const cancelled = await cancelActionRequest("/", "request / one");

  assert.equal(cancelled.status, "cancelling");
  assert.equal(calls[0]?.url, "/api/action-requests/request%20%2F%20one");
  assert.equal(
    calls[1]?.url,
    "/api/action-requests/request%20%2F%20one/cancel",
  );
  assert.equal(calls[1]?.init?.method, "POST");
  assert.equal(calls[1]?.init?.credentials, "same-origin");
});
