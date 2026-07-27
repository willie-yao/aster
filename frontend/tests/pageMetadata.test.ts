import assert from "node:assert/strict";
import { test } from "node:test";

import {
  documentTitleForPath,
  pageTitleForPath,
} from "../src/lib/pageMetadata.js";

test("known routes receive route-specific page titles", () => {
  const cases = [
    ["/", "Overview"],
    ["/flaky", "Test Analysis"],
    ["/flaky/", "Test Analysis"],
    ["/analysis-traces", "Analysis Traces"],
    ["/job/periodic-capz", "Job Details"],
    ["/job/periodic-capz/test/TestCluster", "Test Details"],
    ["/action-request/request-1", "Action Request"],
  ] as const;

  for (const [pathname, expected] of cases) {
    assert.equal(pageTitleForPath(pathname), expected, pathname);
  }
});

test("unknown and malformed routes receive the Not Found title", () => {
  for (const pathname of [
    "/missing",
    "/job",
    "/job/example/extra",
    "/job/example/test",
    "/action-request",
  ]) {
    assert.equal(pageTitleForPath(pathname), "Page Not Found", pathname);
  }
});

test("document titles combine the route title with dashboard branding", () => {
  assert.equal(
    documentTitleForPath("/flaky", "CAPZ Prow Dashboard"),
    "Test Analysis | CAPZ Prow Dashboard",
  );
  assert.equal(
    documentTitleForPath("/does-not-exist", "CAPZ Prow Dashboard"),
    "Page Not Found | CAPZ Prow Dashboard",
  );
});
