import assert from "node:assert/strict";
import { test } from "node:test";

import { navDestinations } from "../src/lib/navigation.js";

const base = {
  pathname: "/",
  pullRequestsEnabled: true,
  analysisHealthEnabled: true,
  aiUsageEnabled: true,
  operatorAccess: true,
};

test("signed-out visitors are only offered destinations that render data", () => {
  // A deployment can advertise operator features while the viewer is anonymous.
  // The flags alone must not surface a destination that only renders a sign-in
  // wall, which is the regression this rail was built to remove.
  const anonymous = navDestinations({ ...base, operatorAccess: false });
  assert.deepEqual(
    anonymous.map((d) => d.id),
    ["overview", "flaky", "pull-requests"],
  );
  // Every operator destination is a sign-in wall without capabilities, so none
  // may appear.
  assert.equal(
    anonymous.some((d) => d.scope === "operator"),
    false,
  );
});

test("operator destinations need the capability flag and an operator session", () => {
  for (const id of ["analysis-health", "ai-usage"]) {
    const flagOnly = navDestinations({ ...base, operatorAccess: false });
    assert.equal(flagOnly.some((d) => d.id === id), false, `${id} must require a session`);
  }

  const healthOnly = navDestinations({ ...base, aiUsageEnabled: false });
  assert.deepEqual(
    healthOnly.filter((d) => d.scope === "operator").map((d) => d.id),
    ["analysis-health"],
  );

  const usageOnly = navDestinations({ ...base, analysisHealthEnabled: false });
  assert.deepEqual(
    usageOnly.filter((d) => d.scope === "operator").map((d) => d.id),
    ["ai-usage"],
  );

  const none = navDestinations({
    ...base,
    analysisHealthEnabled: false,
    aiUsageEnabled: false,
  });
  assert.equal(none.filter((d) => d.scope === "operator").length, 0);
});

test("pull requests follow the manifest, not the capability flags", () => {
  const withoutPulls = navDestinations({ ...base, pullRequestsEnabled: false });
  assert.equal(
    withoutPulls.some((d) => d.id === "pull-requests"),
    false,
  );
});

test("the bottom bar never has to render more than five destinations", () => {
  // The bar gives each destination an equal share of a phone-width viewport,
  // which stops being legible past five.
  assert.ok(navDestinations(base).length <= 5);
});

test("exactly one destination is active per route", () => {
  const routes = [
    ["/", "overview"],
    ["/flaky", "flaky"],
    ["/pull-requests", "pull-requests"],
    ["/analysis-health", "analysis-health"],
    ["/ai-usage", "ai-usage"],
  ] as const;
  for (const [pathname, expected] of routes) {
    const active = navDestinations({ ...base, pathname }).filter((d) => d.active);
    assert.equal(active.length, 1, `${pathname} should mark one destination active`);
    assert.equal(active[0].id, expected);
  }
});

test("nested routes keep their section highlighted", () => {
  const onTest = navDestinations({ ...base, pathname: "/flaky/some-test" });
  assert.equal(onTest.find((d) => d.active)?.id, "flaky");

  const onPull = navDestinations({ ...base, pathname: "/pull-requests/6209" });
  assert.equal(onPull.find((d) => d.active)?.id, "pull-requests");

  const onShared = navDestinations({ ...base, pathname: "/pull-requests/shared/abc" });
  assert.equal(onShared.find((d) => d.active)?.id, "pull-requests");
});

test("job and test detail routes stay under Overview", () => {
  for (const pathname of ["/job/capz-e2e", "/job/capz-e2e/test/It%20works"]) {
    assert.equal(navDestinations({ ...base, pathname }).find((d) => d.active)?.id, "overview");
  }
});

test("a revoked section is dropped without falsely activating another", () => {
  // Signing out while on an operator route removes that destination. No other
  // destination may claim to be the current page, so the rail shows no active
  // item rather than lighting up Overview for a route it does not own.
  const revoked = navDestinations({
    ...base,
    pathname: "/ai-usage",
    operatorAccess: false,
  });
  assert.equal(revoked.some((d) => d.id === "ai-usage"), false);
  assert.equal(revoked.filter((d) => d.active).length, 0);
});
