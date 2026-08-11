import assert from "node:assert/strict";
import { test } from "node:test";

import {
  documentTitleForPath,
  pageTitleForPath,
} from "../src/lib/pageMetadata.js";

test("known routes receive route-specific page titles", () => {
  const cases = [
    ["/", "Overview"],
    ["/flaky", "Failure trends"],
    ["/flaky/", "Failure trends"],
    ["/FLAKY", "Failure trends"],
    ["/analysis-traces", "Analysis traces"],
    ["/ANALYSIS-TRACES", "Analysis traces"],
    ["/ai-usage", "AI usage"],
    ["/AI-USAGE", "AI usage"],
    ["/job/periodic-capz", "Job details"],
    ["/JOB/periodic-capz", "Job details"],
    ["/job/periodic-capz/test/TestCluster", "Test details"],
    ["/job/periodic%2Fcapz/test/Test%20Cluster", "Test details"],
    ["/JOB/periodic-capz/TEST/TestCluster", "Test details"],
    ["/job/periodic-capz/build/123/failure", "Build failure"],
    ["/action-request/request-1", "Draft review"],
    ["/action-request/request%2Fwith%20spaces", "Draft review"],
    ["/ACTION-REQUEST/request-1", "Draft review"],
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
    assert.equal(pageTitleForPath(pathname), "Page not found", pathname);
  }
});

test("document titles combine the route title with dashboard branding", () => {
  assert.equal(
    documentTitleForPath("/flaky", "CAPZ Prow Dashboard"),
    "Failure trends | CAPZ Prow Dashboard",
  );
  assert.equal(
    documentTitleForPath("/does-not-exist", "CAPZ Prow Dashboard"),
    "Page not found | CAPZ Prow Dashboard",
  );
});
