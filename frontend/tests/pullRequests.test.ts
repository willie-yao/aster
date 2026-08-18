import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import {
  attributionLabel,
  attributionTone,
  checkState,
  checkStatusLabel,
  checkSummaryLine,
  filterPullRequests,
  orderPullRequests,
  pullRequestStateCounts,
  pullRequestStateFromParam,
  shortSHA,
  needsInvestigation,
  staleCheckCount,
  unexplainedCount,
  withPullRequestState,
} from "../src/lib/pullRequests.js";
import { pullRequestPath, pullRequestsPath } from "../src/lib/routes.js";
import { pageTitleForPath } from "../src/lib/pageMetadata.js";
import type {
  AttributionVerdict,
  PullRequestCIState,
  PullRequestCheck,
  PullRequestFailure,
  PullRequestSummary,
} from "../src/types/pullRequests.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

function summary(
  number: number,
  ci_state: PullRequestCIState,
  updated_at = "2026-08-17T10:00:00Z",
): PullRequestSummary {
  return {
    number,
    title: `pull ${number}`,
    author: "octocat",
    repo: "example/project",
    base_ref: "main",
    head_sha: "1111111111111111111111111111111111111111",
    html_url: `https://github.com/example/project/pull/${number}`,
    created_at: "2026-08-01T10:00:00Z",
    updated_at,
    ci_state,
    checks_observed: 3,
    checks_failing: ci_state === "FAILING" ? 1 : 0,
    failing_tests: ci_state === "FAILING" ? 2 : 0,
  };
}

function check(overrides: Partial<PullRequestCheck> = {}): PullRequestCheck {
  return {
    job_name: "pull-e2e",
    job_id: "example/project/pull-e2e",
    build_id: "100",
    passed: false,
    started: "2026-08-17T09:00:00Z",
    finished: "2026-08-17T09:30:00Z",
    ...overrides,
  };
}

test("state filter only accepts known presubmit states", () => {
  assert.equal(pullRequestStateFromParam("FAILING"), "FAILING");
  assert.equal(pullRequestStateFromParam("failing"), "FAILING");
  assert.equal(pullRequestStateFromParam("PENDING"), "PENDING");
  assert.equal(pullRequestStateFromParam("UNKNOWN"), "UNKNOWN");
  assert.equal(pullRequestStateFromParam(null), "ALL");
  assert.equal(pullRequestStateFromParam(""), "ALL");
  assert.equal(pullRequestStateFromParam("../../etc/passwd"), "ALL");
});

test("the default state is dropped from the query string", () => {
  const withState = withPullRequestState(new URLSearchParams(), "FAILING");
  assert.equal(withState.get("state"), "FAILING");
  assert.equal(withPullRequestState(withState, "ALL").has("state"), false);
});

test("unrelated query parameters survive a state change", () => {
  const params = new URLSearchParams("run=123&state=PASSING");
  const next = withPullRequestState(params, "FAILING");
  assert.equal(next.get("run"), "123");
  assert.equal(next.get("state"), "FAILING");
});

test("filtering keeps only the requested state", () => {
  const pulls = [summary(1, "FAILING"), summary(2, "PASSING"), summary(3, "PENDING")];
  assert.deepEqual(
    filterPullRequests(pulls, "FAILING").map((pull) => pull.number),
    [1],
  );
  assert.equal(filterPullRequests(pulls, "ALL").length, 3);
});

test("state counts cover every state plus the unfiltered total", () => {
  const counts = pullRequestStateCounts([
    summary(1, "FAILING"),
    summary(2, "FAILING"),
    summary(3, "PASSING"),
    summary(4, "UNKNOWN"),
  ]);
  assert.equal(counts.ALL, 4);
  assert.equal(counts.FAILING, 2);
  assert.equal(counts.PASSING, 1);
  assert.equal(counts.PENDING, 0);
  assert.equal(counts.UNKNOWN, 1);
});

test("ordering leads with failures then the most recently updated", () => {
  const ordered = orderPullRequests([
    summary(1, "PASSING", "2026-08-17T12:00:00Z"),
    summary(2, "UNKNOWN", "2026-08-17T13:00:00Z"),
    summary(3, "FAILING", "2026-08-16T09:00:00Z"),
    summary(4, "PENDING", "2026-08-17T11:00:00Z"),
    summary(5, "FAILING", "2026-08-17T10:00:00Z"),
  ]);
  assert.deepEqual(
    ordered.map((pull) => pull.number),
    [5, 3, 4, 1, 2],
  );
});

test("ordering does not mutate its input", () => {
  const pulls = [summary(1, "PASSING"), summary(2, "FAILING")];
  orderPullRequests(pulls);
  assert.deepEqual(
    pulls.map((pull) => pull.number),
    [1, 2],
  );
});

test("an unfinished build is never presented as a pass", () => {
  assert.equal(checkState(check({ finished: undefined, passed: true })), "RUNNING");
  assert.equal(checkStatusLabel(check({ finished: undefined, passed: true })), "Running");
  assert.equal(checkState(check({ passed: true })), "PASSING");
  assert.equal(checkState(check({ passed: false })), "FAILING");
});

test("check summaries distinguish no reported failure from several", () => {
  assert.equal(checkSummaryLine(check({ finished: undefined })), "Still running");
  assert.equal(checkSummaryLine(check({ passed: true })), "Passed");
  assert.equal(checkSummaryLine(check({ tests_failed: 0 })), "Failed without reporting a failing test");
  assert.equal(checkSummaryLine(check({ tests_failed: 1 })), "1 failing test");
  assert.equal(checkSummaryLine(check({ tests_failed: 4 })), "4 failing tests");
  assert.equal(
    checkSummaryLine(
      check({ tests_failed: 60, failures_truncated: true, failures: new Array(50).fill(null).map(() => ({
        name: "TestX",
        status: "failed" as const,
        duration_seconds: 0,
      })) }),
    ),
    "60 failing tests, showing the first 50",
  );
});

test("stale checks are counted for the detail summary", () => {
  assert.equal(staleCheckCount([check({ stale: true }), check(), check({ stale: true })]), 2);
});

test("commit display is abbreviated without inventing a value", () => {
  assert.equal(shortSHA("1111111111111111111111111111111111111111"), "1111111");
  assert.equal(shortSHA("abc"), "abc");
  assert.equal(shortSHA(undefined), "");
  assert.equal(shortSHA("  "), "");
});

test("pull request routes stay encoded inside same-origin app paths", () => {
  assert.equal(pullRequestsPath(), "/pull-requests");
  assert.equal(pullRequestPath(6209), "/pull-requests/6209");
});

test("pull request routes receive route-specific page titles", () => {
  assert.equal(pageTitleForPath("/pull-requests"), "Pull Requests");
  assert.equal(pageTitleForPath("/PULL-REQUESTS"), "Pull Requests");
  assert.equal(pageTitleForPath("/pull-requests/6209"), "Pull request checks");
  assert.equal(pageTitleForPath("/pull-requests//6209"), "Page not found");
  assert.equal(pageTitleForPath("/pull-requests/6209/extra"), "Page not found");
});

test("the pull request nav tab and routes are gated on the manifest", () => {
  const layout = source("src/components/Layout.tsx");
  const app = source("src/App.tsx");

  assert.match(layout, /manifest\.pull_requests\?\.enabled \?\? false/);
  assert.match(layout, /\{pullRequestsEnabled && \(/);
  assert.match(layout, /label="Pull Requests"/);
  // The overview tab must not stay active while a pull request route is shown.
  assert.match(layout, /overviewActive = !flakyActive && !pullRequestsActive/);
  assert.match(app, /path="pull-requests" element=\{<PullRequestsPage \/>\}/);
  assert.match(app, /path="pull-requests\/:number"/);
});

test("the pull request ledger follows the Overview structural language", () => {
  const ledger = source("src/components/PullRequestLedger.tsx");

  assert.match(ledger, /overviewLayout\.ledgerRowMinHeight/);
  assert.match(ledger, /overviewTypography\.tableHeading/);
  assert.match(ledger, /bgcolor: "surface\.container"/);
  assert.match(ledger, /"&:hover": \{ bgcolor: "surface\.containerHigh" \}/);
  assert.match(ledger, /boxShadow: "inset 2px 0 0 var\(--mui-palette-primary-main\)"/);
  // Desktop table and mobile list are mutually exclusive, as on the Overview.
  assert.match(ledger, /display: \{ xs: "none", lg: "block" \}/);
  assert.match(ledger, /display: \{ xs: "block", lg: "none" \}/);
  assert.match(ledger, /role="table"/);
  assert.match(ledger, /role="columnheader"/);
});

test("pull request pages reuse the shared loading, error, and detail chrome", () => {
  const list = source("src/pages/PullRequestsPage.tsx");
  const detail = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(list, /<LoadingState \/>/);
  assert.match(list, /<ErrorState/);
  assert.match(list, /overviewTypography\.pageHeadline/);
  // A disabled consumer gets an explanation instead of a stuck spinner.
  assert.match(list, /Pull request triage is not enabled/);

  assert.match(detail, /<DetailSectionBand/);
  assert.match(detail, /<RunMetadata/);
  assert.match(detail, /← Pull requests/);
  assert.match(detail, /fontSize: \{ xs: "26px", sm: "30px" \}/);
  assert.match(detail, /lineHeight: \{ xs: "33px", sm: "38px" \}/);
  // Retention gaps are explained rather than shown as an empty section.
  assert.match(detail, /retention window/);
});

test("presubmit job links are gated on presubmits being published as jobs", () => {
  const detail = source("src/pages/PullRequestDetailPage.tsx");

  // Without source.include_presubmits there is no job detail file for a
  // presubmit, so the job name must render as plain text instead of a dead link.
  assert.match(detail, /manifest\.source\?\.include_presubmits \?\? false/);
  assert.match(detail, /if \(!linkToJob\) \{/);
  assert.match(detail, /linkToJob=\{linkToJob\}/);
  assert.equal(detail.match(/to=\{jobPath\(check\.job_id\)\}/g)?.length, 1);
});

test("failing counts name their units instead of an ambiguous ratio", () => {
  const ledger = source("src/components/PullRequestLedger.tsx");

  assert.doesNotMatch(ledger, /\$\{pull\.checks_failing\} \/ \$\{pull\.failing_tests\}/);
  assert.match(ledger, /checks_failing === 1 \? "job" : "jobs"/);
  assert.match(ledger, /failing_tests === 1 \? "test" : "tests"/);
});

test("the whole ledger row is one clickable target", () => {
  const ledger = source("src/components/PullRequestLedger.tsx");

  // A stretched pseudo-element covers the row, so the row must establish the
  // containing block for it.
  assert.equal(ledger.match(/position: "relative"/g)?.length, 2);
  assert.match(ledger, /"&::after": \{[\s\S]*?position: "absolute"[\s\S]*?inset: 0/);
  assert.match(ledger, /cursor: "pointer"/);
  // One link per row keeps the accessibility tree clean, and its name carries
  // the title rather than a bare number.
  assert.match(ledger, /aria-label=\{`Pull request \$\{pull\.number\}: \$\{pull\.title \|\| "Untitled"\}`\}/);
  assert.equal(ledger.match(/component=\{RouterLink\}/g)?.length, 1);
  assert.match(ledger, /"&:focus-visible::after"/);
});

test("a failing test only becomes a disclosure when there is output to reveal", () => {
  const detail = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(detail, /const body = failure\.failure_body\?\.trim\(\)/);
  assert.match(detail, /\{body \? \(/);
  assert.match(detail, /aria-expanded=\{open\}/);
  assert.match(detail, /aria-controls=\{bodyID\}/);
  assert.match(detail, /\{open \? "Hide output" : "Show output"\}/);
  assert.match(detail, /\{body && open && \(/);
});

function attributed(verdict: AttributionVerdict | undefined): PullRequestFailure {
  const base: PullRequestFailure = { name: "TestA", status: "failed", duration_seconds: 0 };
  if (!verdict) return base;
  return { ...base, attribution: { verdict, confidence: "high", summary: "" } };
}

test("verdict labels never assert that a pull request caused a failure", () => {
  const labels: AttributionVerdict[] = [
    "pre_existing",
    "widespread",
    "known_flake",
    "touches_changed_code",
    "unexplained",
    "inconclusive",
  ];
  for (const verdict of labels) {
    const label = attributionLabel(verdict);
    assert.ok(label.length > 0, verdict);
    assert.doesNotMatch(label, /caused|broke|your fault/i, verdict);
  }
  assert.equal(attributionLabel("pre_existing"), "Already failing on base");
  assert.equal(attributionLabel("widespread"), "Not this PR");
  assert.equal(attributionLabel("unexplained"), "Needs investigation");
  // Overlap is descriptive, not causal.
  assert.equal(attributionLabel("touches_changed_code"), "Touches changed code");
});

test("verdicts that rule the pull request out are not styled as errors", () => {
  assert.equal(attributionTone("pre_existing"), "info");
  assert.equal(attributionTone("widespread"), "info");
  assert.equal(attributionTone("known_flake"), "warning");
  assert.equal(attributionTone("touches_changed_code"), "error");
  assert.equal(attributionTone("unexplained"), "error");
  assert.equal(attributionTone("inconclusive"), "default");
});

test("only failures the baseline could not rule out need investigation", () => {
  assert.equal(needsInvestigation(attributed("unexplained")), true);
  assert.equal(needsInvestigation(attributed("inconclusive")), true);
  assert.equal(needsInvestigation(attributed("pre_existing")), false);
  assert.equal(needsInvestigation(attributed("widespread")), false);
  assert.equal(needsInvestigation(attributed("known_flake")), false);
  // Overlap sharpens a residual failure rather than resolving it.
  assert.equal(needsInvestigation(attributed("touches_changed_code")), true);
  // An unattributed failure has not been ruled out, so it still counts.
  assert.equal(needsInvestigation(attributed(undefined)), true);
});

test("the investigation count sums across every check", () => {
  const checks: PullRequestCheck[] = [
    {
      job_name: "a", job_id: "a", build_id: "1", passed: false, started: "",
      failures: [attributed("unexplained"), attributed("pre_existing")],
    },
    {
      job_name: "b", job_id: "b", build_id: "2", passed: false, started: "",
      failures: [attributed("widespread"), attributed("inconclusive")],
    },
    { job_name: "c", job_id: "c", build_id: "3", passed: true, started: "" },
  ];
  assert.equal(unexplainedCount(checks), 2);
  assert.equal(unexplainedCount([]), 0);
});

test("the attribution banner leads each failure and cites its evidence", () => {
  const detail = source("src/pages/PullRequestDetailPage.tsx");

  assert.match(detail, /function AttributionBanner/);
  assert.match(detail, /<AttributionBanner attribution=\{failure\.attribution\}/);
  assert.match(detail, /attribution\.evidence\?\.map/);
  assert.match(detail, /\{attribution\.confidence\} confidence/);
  // The banner precedes the failure output so a ruled-out failure reads first.
  assert.ok(
    detail.indexOf("<AttributionBanner") < detail.indexOf("{failure.failure_message &&"),
  );
});

test("outbound pull request links cannot reach the opener", () => {
  const detail = source("src/pages/PullRequestDetailPage.tsx");
  const externalLinks = detail.match(/target="_blank"/g) ?? [];
  const guarded = detail.match(/rel="noopener noreferrer"/g) ?? [];
  assert.ok(externalLinks.length > 0);
  assert.equal(guarded.length, externalLinks.length);
});
