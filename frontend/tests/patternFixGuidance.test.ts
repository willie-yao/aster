import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { causalGroupFixTarget, patternFixGuidanceBuildID } from "../src/lib/patternFixGuidance.js";
import type { BuildResult, PatternAnalysis, PatternCausalGroup } from "../src/types/dashboard.js";

function source(path: string): string {
  return readFileSync(resolve(process.cwd(), path), "utf8");
}

const causalPattern: PatternAnalysis = {
  id: "pattern-id",
  content_hash: "pattern-hash",
  subject: "several failures",
  generated_at: "2026-08-14T00:00:00Z",
  builds_analyzed: 3,
  recurrence_classification: "shared_cause",
  causal_groups: [
    { builds: ["208060", "208726"], root_cause: "first cause", confidence: "high" },
    { builds: ["209114"], root_cause: "second cause", confidence: "medium" },
  ],
  systemic: true,
  confidence: "high",
  summary: "Several causes recur.",
};

const junitRun: BuildResult = {
  build_id: "208060",
  job_name: "job",
  started: "2026-08-14T00:00:00Z",
  finished: "2026-08-14T00:01:00Z",
  passed: false,
  result: "FAILURE",
  duration_seconds: 60,
  commit: "abcdef12",
  prow_url: "https://prow.example/208060",
  web_url: "https://storage.example/208060",
  build_log_url: "https://storage.example/208060/build-log.txt",
  test_cases: [{ name: "fails", status: "failed", duration_seconds: 1, junit_file: "junit_01.xml" }],
  tests_total: 1,
  tests_passed: 0,
  tests_failed: 1,
  tests_skipped: 0,
};

const analysis = {
  generated_at: "2026-08-14T00:02:00Z",
  model: "model",
  root_cause: "cause",
  severity: "high",
  suggested_fix: "fix",
};

const groundedRun: BuildResult = {
  ...junitRun,
  test_cases: [
    {
      name: "fails",
      status: "failed",
      duration_seconds: 1,
      junit_file: "junit_01.xml",
      ai_analysis: { ...analysis, file_links: { "pkg/thing.go": "https://github.com/o/r/blob/4f2a9c1e83b7d0526ab1c94f7e3d81a06b5c2f97/pkg/thing.go" } },
    },
  ],
};

const firstGroup = causalPattern.causal_groups![0];
const singleBuildGroup = causalPattern.causal_groups![1];

test("causal guidance selects an affected build with a failed JUnit test", () => {
  assert.equal(patternFixGuidanceBuildID(causalPattern, [junitRun]), "208060");
  assert.equal(patternFixGuidanceBuildID({ ...causalPattern, causal_groups: [] }, [junitRun]), null);
  assert.equal(patternFixGuidanceBuildID({ ...causalPattern, recurrence_classification: undefined }, [junitRun]), null);
  assert.equal(patternFixGuidanceBuildID(causalPattern, [{ ...junitRun, test_cases: [] }]), null);
  assert.equal(patternFixGuidanceBuildID(causalPattern, [{ ...junitRun, build_id: "unrelated" }]), null);
  assert.equal(
    patternFixGuidanceBuildID(causalPattern, [
      {
        ...junitRun,
        test_cases: [{ name: "passes", status: "passed", duration_seconds: 1, junit_file: "junit_01.xml" }],
      },
    ]),
    null,
  );
});

test("a cause routes to a failed test that can actually start a Fix investigation", () => {
  assert.deepEqual(causalGroupFixTarget(firstGroup, [groundedRun]), {
    buildID: "208060",
    testName: "fails",
  });
});

test("a failed test with no published file link is conclusively ineligible", () => {
  assert.equal(causalGroupFixTarget(firstGroup, [junitRun]), null);
  assert.equal(
    causalGroupFixTarget(firstGroup, [
      { ...groundedRun, test_cases: [{ ...groundedRun.test_cases[0], ai_analysis: { ...analysis, file_links: {} } }] },
    ]),
    null,
  );
});

test("build-sourced and JUnit-less failures never become fix targets", () => {
  assert.equal(
    causalGroupFixTarget(firstGroup, [
      { ...groundedRun, test_cases: [{ ...groundedRun.test_cases[0], source: "build" }] },
    ]),
    null,
  );
  assert.equal(
    causalGroupFixTarget(firstGroup, [
      { ...groundedRun, test_cases: [{ ...groundedRun.test_cases[0], junit_file: undefined }] },
    ]),
    null,
  );
  assert.equal(
    causalGroupFixTarget(firstGroup, [
      { ...groundedRun, test_cases: [{ ...groundedRun.test_cases[0], status: "passed" }] },
    ]),
    null,
  );
});

test("a cause only considers its own builds", () => {
  assert.equal(causalGroupFixTarget(singleBuildGroup, [groundedRun]), null);
  assert.deepEqual(causalGroupFixTarget(singleBuildGroup, [{ ...groundedRun, build_id: "209114" }]), {
    buildID: "209114",
    testName: "fails",
  });
});

test("a repeated test name only routes when the first occurrence is the eligible one", () => {
  const eligible = groundedRun.test_cases[0];
  const retriedPass = { ...eligible, status: "passed" as const, ai_analysis: undefined };

  // The detail page opens the first occurrence, so a shadowed eligible retry is
  // unreachable and must not be advertised.
  assert.equal(
    causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [retriedPass, eligible] }]),
    null,
  );
  assert.deepEqual(causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [eligible, retriedPass] }]), {
    buildID: "208060",
    testName: "fails",
  });
});

test("an occurrence hidden from the ledger still shadows the routing target", () => {
  const eligible = groundedRun.test_cases[0];
  const skipped = { ...eligible, status: "skipped" as const, ai_analysis: undefined };

  // executedResultTests drops skipped cases, but the detail page does not, so
  // eligibility must be judged against the raw occurrence order.
  assert.equal(
    causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [skipped, eligible] }]),
    null,
  );
});

test("a single-build cause still gets the per-test fix route", () => {
  const singleBuild: PatternCausalGroup = { builds: ["209114"], root_cause: "second", confidence: "medium" };
  assert.deepEqual(causalGroupFixTarget(singleBuild, [{ ...groundedRun, build_id: "209114" }]), {
    buildID: "209114",
    testName: "fails",
  });
});

test("fix routing sits with each cause and stays behind the chat capabilities", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const routing = source("src/components/CausalGroupFixRouting.tsx");

  assert.match(banner, /const fixCapable = Boolean\(features\.analysis_chat && features\.junit_chat_fix\)/);
  assert.match(banner, /causalGroups\.map\(\(group, index\)[\s\S]*<CausalGroupFixRouting jobID=\{jobID\} target=\{causalFixTargets\[index\]\} \/>/);
  assert.match(routing, /testRunPath\(jobID, target\.testName, target\.buildID\)/);
  assert.match(routing, /Open test for Fix investigation/);
  assert.match(routing, /No failed JUnit test in these builds meets the Fix investigation requirements/);
});

test("the pattern-level panel is a fallback for causes with no eligible test", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const guidance = source("src/components/PatternFixGuidance.tsx");

  assert.match(banner, /const showFixGuidance = Boolean\(jobID && fixGuidanceBuildID && fixCapable && !hasCausalFixTarget\)/);
  assert.match(banner, /<PatternFixGuidance jobID=\{jobID\} buildID=\{fixGuidanceBuildID\} \/>/);
  assert.ok(banner.indexOf("<PatternFixGuidance") < banner.indexOf("<AnalysisChat"));
  assert.equal(banner.match(/<PatternFixGuidance/g)?.length, 1);
  assert.match(guidance, /Fix investigation unavailable/);
  assert.match(guidance, /No failed JUnit test in the affected builds meets the Fix investigation requirements/);
  assert.match(guidance, /View failed tests/);
  assert.match(guidance, /jobRunPath\(jobID, buildID\)/);
  assert.match(guidance, /to=\{destination\}/);
  assert.match(guidance, /location\.hash === `#\$\{failedTestGridID\}`/);
  assert.match(guidance, /\[aria-controls="\$\{failedTestGridID\}"\]/);
  assert.match(guidance, /toggle\.click\(\)/);
  assert.match(guidance, /scrollIntoView\(\{ block: "start" \}\)/);
  assert.doesNotMatch(guidance, /useAuth|authStatus|authenticated/);
});

test("causal build links keep exact IDs and expose clear accessible names", () => {
  const banner = source("src/components/PatternBanner.tsx");

  assert.match(banner, /Affected \{group\.builds\.length === 1 \? "build" : "builds"\}/);
  assert.match(banner, /aria-label=\{`Open affected build \$\{buildID\}`\}/);
  assert.match(banner, />\s*\{buildID\}\s*<\/Link>/);
  assert.match(banner, /to=\{jobID \? jobRunPath\(jobID, buildID\) : "#"\}/);
});

test("causal actions stay blocked while pattern chat and exact-JUnit Fix remain unchanged", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const chat = source("src/components/AnalysisChat.tsx");

  assert.match(banner, /!analysisOnly && isCurrent && lifecycleActive && pattern\.systemic && pattern\.id && \(/);
  assert.match(banner, /<FailureActions/);
  assert.doesNotMatch(banner, />\s*Draft issue\s*</);
  assert.doesNotMatch(banner, />\s*Draft fix PR\s*</);
  assert.match(banner, /<AnalysisChat/);
  assert.match(chat, /Start fix investigation/);
});

test("guidance keeps a contained mobile action and points to the nearby test ledger", () => {
  const guidance = source("src/components/PatternFixGuidance.tsx");
  const routing = source("src/components/CausalGroupFixRouting.tsx");
  const grid = source("src/components/TestResultsGrid.tsx");

  assert.match(guidance, /minWidth: 0/);
  assert.match(guidance, /maxWidth: "100%"/);
  assert.match(guidance, /minHeight: 44/);
  assert.match(guidance, /width: \{ xs: "100%", sm: "auto" \}/);
  assert.match(routing, /minHeight: \{ xs: 44, sm: 32 \}/);
  assert.match(grid, /Choose a failed test from the Test results section below/);
});
