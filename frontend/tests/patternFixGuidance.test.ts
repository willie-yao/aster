import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { patternFixGuidanceBuildID } from "../src/lib/patternFixGuidance.js";
import type { BuildResult, PatternAnalysis } from "../src/types/dashboard.js";

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

test("guidance is outside pattern chat and targets the affected build grid", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const guidance = source("src/components/PatternFixGuidance.tsx");

  assert.match(banner, /<PatternFixGuidance jobID=\{jobID\} buildID=\{fixGuidanceBuildID\} \/>/);
  assert.ok(banner.indexOf("<PatternFixGuidance") < banner.indexOf("<AnalysisChat"));
  assert.equal(banner.match(/<PatternFixGuidance/g)?.length, 1);
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
  const grid = source("src/components/TestResultsGrid.tsx");

  assert.match(guidance, /minWidth: 0/);
  assert.match(guidance, /maxWidth: "100%"/);
  assert.match(guidance, /minHeight: 44/);
  assert.match(guidance, /width: \{ xs: "100%", sm: "auto" \}/);
  assert.match(grid, /Choose a failed test from the Test results section below/);
});
