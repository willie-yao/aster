import assert from "node:assert/strict";
import { test } from "node:test";

import {
  documentTitleForPath,
  pageTitleForPath,
} from "../src/lib/pageMetadata.js";

test("known routes receive route-specific page titles", () => {
  const cases = [
    ["/", "Overview"],
    ["/flaky", "Failure Analysis"],
    ["/flaky/", "Failure Analysis"],
    ["/FLAKY", "Failure Analysis"],
    ["/analysis-traces", "Analysis Traces"],
    ["/ANALYSIS-TRACES", "Analysis Traces"],
    ["/ai-usage", "AI Usage"],
    ["/AI-USAGE", "AI Usage"],
    ["/job/periodic-capz", "Job Details"],
    ["/JOB/periodic-capz", "Job Details"],
    ["/job/periodic-capz/test/TestCluster", "Test Details"],
    ["/job/periodic%2Fcapz/test/Test%20Cluster", "Test Details"],
    ["/JOB/periodic-capz/TEST/TestCluster", "Test Details"],
    ["/job/periodic-capz/build/123/failure", "Build Failure"],
    ["/action-request/request-1", "Action Request"],
    ["/action-request/request%2Fwith%20spaces", "Action Request"],
    ["/ACTION-REQUEST/request-1", "Action Request"],
  ] as const;

  for (const [pathname, expected] of cases) {
    assert.equal(pageTitleForPath(pathname), expected, pathname);
  }
});

test("unknown and malformed routes receive the Not Found title", () => {
  for (const pathname of [
    "/missing",
    "/job",
    "/job//periodic-capz",
    "/job/example/extra",
    "/job/example/test",
    "/job/periodic-capz//test/TestCluster",
    "/action-request",
    "/action-request//request-1",
    "//evil.example/path",
    "/\\evil.example/path",
  ]) {
    assert.equal(pageTitleForPath(pathname), "Page Not Found", pathname);
  }
});

test("document titles combine the route title with dashboard branding", () => {
  assert.equal(
    documentTitleForPath("/flaky", "CAPZ Prow Dashboard"),
    "Failure Analysis | CAPZ Prow Dashboard",
  );
  assert.equal(
    documentTitleForPath("/does-not-exist", "CAPZ Prow Dashboard"),
    "Page Not Found | CAPZ Prow Dashboard",
  );
});
