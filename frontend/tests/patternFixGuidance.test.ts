import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { causalGroupEvidencePresent, causalGroupFixTarget, patternFixGuidanceBuildID } from "../src/lib/patternFixGuidance.js";
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
  disposition: "citations_verified" as const,
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

test("a cause routes to a failed test that can actually start a fix proposal", () => {
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

test("a cause routes to the representative failure it was actually built from", () => {
  const eligible = groundedRun.test_cases[0];
  const higherSeverity = {
    ...eligible,
    name: "other",
    ai_analysis: { ...analysis, severity: "critical", file_links: {} },
  };

  // The causal group's root cause comes from the highest-severity analyzed
  // failure, so an unrelated eligible failure must not be offered in its place.
  assert.equal(
    causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [higherSeverity, eligible] }]),
    null,
  );
  assert.deepEqual(
    causalGroupFixTarget(firstGroup, [
      { ...groundedRun, test_cases: [{ ...higherSeverity, ai_analysis: { ...analysis, severity: "low", file_links: {} } }, eligible] },
    ]),
    { buildID: "208060", testName: "fails" },
  );
});

test("an unanalyzed failure never becomes the representative", () => {
  const eligible = groundedRun.test_cases[0];
  const unanalyzed = { name: "other", status: "failed" as const, duration_seconds: 1, junit_file: "junit_01.xml" };

  assert.deepEqual(causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [unanalyzed, eligible] }]), {
    buildID: "208060",
    testName: "fails",
  });
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

  // The detail page opens the first occurrence, so a shadowed representative is
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

  // The ledger drops skipped cases, but the detail page does not, so
  // reachability must be judged against the raw occurrence order.
  assert.equal(
    causalGroupFixTarget(firstGroup, [{ ...groundedRun, test_cases: [skipped, eligible] }]),
    null,
  );
});

test("a cause falls through to another affected build when one has no reachable target", () => {
  const eligible = groundedRun.test_cases[0];
  const blockedRun = { ...groundedRun, test_cases: [{ ...eligible, ai_analysis: { ...analysis, file_links: {} } }] };

  assert.deepEqual(
    causalGroupFixTarget(firstGroup, [blockedRun, { ...groundedRun, build_id: "208726" }]),
    { buildID: "208726", testName: "fails" },
  );
});

test("a single-build cause still gets the per-test fix route", () => {
  const singleBuild: PatternCausalGroup = { builds: ["209114"], root_cause: "second", confidence: "medium" };
  assert.deepEqual(causalGroupFixTarget(singleBuild, [{ ...groundedRun, build_id: "209114" }]), {
    buildID: "209114",
    testName: "fails",
  });
});

// Ownership must never cost a project-owned failure its Fix route, and an
// eligible verified failure still wins over an upstream note.
test("cause ownership does not change which failures can start a fix proposal", () => {
  const ownRepo: PatternCausalGroup = {
    ...firstGroup,
    cause_location: { repository: "kubernetes-sigs/cluster-api-provider-azure" },
  };
  const upstream: PatternCausalGroup = {
    ...firstGroup,
    cause_location: { repository: "kubernetes/kubernetes", external: true },
  };

  assert.deepEqual(causalGroupFixTarget(ownRepo, [groundedRun]), { buildID: "208060", testName: "fails" });
  assert.deepEqual(causalGroupFixTarget(upstream, [groundedRun]), { buildID: "208060", testName: "fails" });
  assert.equal(causalGroupFixTarget(ownRepo, [junitRun]), null);
});

test("fix routing sits with each cause and stays behind the chat capabilities", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const nextStep = source("src/components/CausalGroupNextStep.tsx");
  const routing = source("src/components/CausalGroupFixRouting.tsx");

  assert.match(banner, /const fixCapable = Boolean\(features\.analysis_chat && features\.junit_chat_fix\)/);
  assert.match(
    banner,
    /causalGroups\.map\(\(group, index\)[\s\S]*<CausalGroupNextStep[\s\S]*routing=\{[\s\S]*fixCapable[\s\S]*target: causalFixTargets\[index\][\s\S]*externalCause: externalCause\(group\.cause_location\)/,
  );
  // The prose half stays in the card body; the action half moved to the bar.
  assert.match(nextStep, /<CausalGroupFixNotice[\s\S]*target=\{routable\.target\}/);
  assert.match(banner, /<CausalGroupFixButton[\s\S]*target=\{causalFixTargets\[index\]\}/);
  assert.match(routing, /testRunPath\(jobID, target\.testName, target\.buildID\)/);
  assert.match(routing, /No failed JUnit test in these builds meets the Fix eligibility requirements/);
});

test("fix routing reads as an action and names the test it opens", () => {
  const routing = source("src/components/CausalGroupFixRouting.tsx");

  // The old treatment was the monospace data token, which is exactly how build
  // ID chips render a few lines above it in the same cause.
  assert.match(routing, /<Button/);
  // Both states read as links; the demotion is carried by the underline and the
  // muted colour rather than by the MUI variant.
  assert.match(routing, /variant="text"/);
  assert.match(routing, /textUnderlineOffset: 3/);
  assert.match(routing, /stale \? <VisibilityOutlined aria-hidden \/> : <AutoFixHigh aria-hidden \/>/);
  assert.doesNotMatch(routing, /overviewTypography\.data/);
  assert.doesNotMatch(routing, /bgcolor: "action\.selected"/);

  // The subject is in the visible label, not in a caption below it that reads
  // as if it belonged to the next cause, and it uses the same humanized title
  // the test ledger shows rather than the raw JUnit name. The label names the
  // navigation it performs: "Fix" read as an action the button never took.
  assert.match(routing, /parseTestDisplayName\(target\.testName\)\.displayName/);
  assert.match(routing, /const actionLabel = "open representative failure"/);
  assert.doesNotMatch(routing, /"View affected failure"|: "Fix"/);
  // The icon carries the verb, so the visible label is only the test; the verb
  // trails the same text in the accessible name.
  assert.match(routing, /^\s*\{testName\}$/m);
  assert.doesNotMatch(routing, /\{target\.testName\} in build \{target\.buildID\}\s*<\/Typography>/);

  // A long test name truncates inline, and the full value stays reachable on
  // hover and on keyboard focus rather than through a hover-only native title.
  assert.match(routing, /textOverflow: "ellipsis"/);
  assert.match(routing, /<Tooltip title=\{accessibleName\}>/);
  assert.match(routing, /aria-label=\{accessibleName\}/);
  assert.doesNotMatch(routing, /title=\{subject\}\s*\n\s*aria-label/);
});

test("the build only joins the label where it is needed to tell two actions apart", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const routing = source("src/components/CausalGroupFixRouting.tsx");

  // Two causes can route to the same test in different builds. Paying 19 digits
  // on every action to cover that case is what made the label unreadable.
  assert.match(routing, /showBuild = false/);
  assert.match(routing, /\{showBuild && \(/);
  assert.match(banner, /showBuild=\{fixTargetNeedsBuild\[index\]\}/);
  assert.match(banner, /stale: !lifecycleActive/);

  // Counting the DISPLAYED label, not the canonical name: two canonical names
  // can humanize to one title, which would hide both builds and leave two
  // identical buttons. causalFixRouting.test.ts proves the rendered result.
  assert.match(banner, /parseTestDisplayName\(target\.testName\)\.displayName/);
  assert.match(banner, /const fixTargetLabelCounts = fixTargetLabels\.reduce/);

  // One suffix backs both strings, and the accessible name leads with the
  // visible subject, so the visible label cannot drift out of being a literal
  // prefix of the accessible name.
  assert.match(routing, /const buildSuffix = ` in build \$\{target\.buildID\}`/);
  assert.match(routing, /const subject = `\$\{testName\}\$\{buildSuffix\}`/);
  assert.match(routing, /const accessibleName = `\$\{subject\}, \$\{actionLabel\}`/);
  assert.match(routing, /whiteSpace: "pre"/);
  assert.doesNotMatch(routing, /\\u00a0/);
});

test("the pattern-level panel is a fallback for causes with no eligible test", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const guidance = source("src/components/PatternFixGuidance.tsx");

  assert.match(banner, /const showFixGuidance = Boolean\(jobID && fixGuidanceBuildID && fixCapable && !hasCausalFixTarget\)/);
  assert.match(banner, /<PatternFixGuidance jobID=\{jobID\} buildID=\{fixGuidanceBuildID\} externalCause=\{patternUpstreamCause\} chatAvailable=\{Boolean\(chatRef\)\} \/>/);
  assert.ok(banner.indexOf("<PatternFixGuidance") < banner.indexOf("<AnalysisChat"));
  assert.equal(banner.match(/<PatternFixGuidance/g)?.length, 1);
  assert.match(guidance, /Fix proposal unavailable/);
  assert.match(guidance, /No failed JUnit test in the affected builds meets the Fix eligibility requirements/);
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

  // Drafting stays tied to the remediation contract; dismissal must not be, and
  // neither gate may suppress the other.
  assert.match(banner, /const draftable = patternDraftable\(pattern, refreshStatus\)/);
  assert.doesNotMatch(banner, /draftable = dismissible/);
  assert.match(banner, /draftable=\{draftable\}/);
  assert.match(banner, /eligibilityHint=\{draftable \? actionEligibility : null\}/);
  assert.match(banner, /<FailureActions/);
  assert.doesNotMatch(banner, />\s*Draft issue\s*</);
  assert.doesNotMatch(banner, />\s*Draft fix PR\s*</);
  assert.match(banner, /<AnalysisChat/);
  assert.match(chat, /Use this finding in a fix proposal/);
});

test("pattern resolution is reachable on the causal-group results the engine publishes", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const actions = source("src/components/FailureActions.tsx");

  // The regression: every published pattern carries a recurrence_classification,
  // so gating the control on !analysisOnly hid it everywhere.
  assert.match(banner, /patternResolvable\(pattern, refreshStatus\)/);
  assert.match(banner, /\{showFailureActions && pattern\.id && \(/);
  assert.doesNotMatch(banner, /!analysisOnly[^\n]*<FailureActions/);
  // The resolved state has to render for the same patterns it can be set on.
  assert.match(banner, /const resolvedEntry = pattern\.id \? resolved\.resolved\[pattern\.id\] : undefined/);
  // A resolved pattern keeps its Reopen control even once a fresh resolution
  // would be refused.
  assert.match(banner, /draftable \|\| canResolve \|\| Boolean\(resolvedEntry\)/);
  assert.match(banner, /canResolve=\{canResolve\}/);
  // Per-cause resolution replaces the pattern-level control only where it
  // covers every cause, so a pattern with an unsigned cause keeps the fallback.
  assert.match(banner, /const causeResolutionCovers = patternResolutionCovered\(pattern, refreshStatus\)/);
  assert.match(banner, /patternResolvable\(pattern, refreshStatus\) && !causeResolutionCovers/);

  // Only drafting is suppressed when draftable is false; resolution is not.
  assert.match(actions, /const drafting = draftable && features\.action_requests/);
  assert.match(actions, /\{drafting && canStartActions && \(/);
  assert.match(actions, /\{draftable && !eligibilityLoading && eligibility/);
});

// A cause is acknowledged on its own, so its control has to live in the cause
// card and key on the signature the server records the resolution under.
test("per-cause resolution is offered per causal group and keyed by signature", () => {
  const banner = source("src/components/PatternBanner.tsx");
  const cause = source("src/components/CauseResolution.tsx");
  const eligibility = source("src/lib/actionEligibility.ts");
  const resolution = source("src/lib/resolution.ts");

  assert.match(banner, /<CauseResolution\s+signature=\{group\.signature\}/);
  assert.match(banner, /resolvedEntry=\{causeResolutions\[index\]\}/);
  assert.match(banner, /resolvable=\{causeResolvableFlags\[index\]\}/);
  assert.match(banner, /group\.signature \? resolved\.causes\[group\.signature\] : undefined/);
  // An unsigned group has no durable key, so it is never offered the control.
  assert.match(eligibility, /group\.signature\?\.trim\(\) &&/);
  // Reopening stays available once a fresh resolution would be refused.
  assert.match(resolution, /signature && \(resolvable \|\| resolved\)/);
  // The control and the action bar ask one gate, so the bar can never draw its
  // rule above a row that renders nothing.
  assert.match(cause, /if \(!causeResolutionAvailable\(\{/);
  assert.match(banner, /causeResolutionAvailable\(\{/);
  // An anonymous viewer needs the pattern-level block for its sign-in prompt
  // wherever a signed-in one would see per-cause controls. A cause that is
  // already resolved still offers Reopen after it stops qualifying for a fresh
  // resolution, so covering only the resolvable ones would drop the prompt.
  assert.match(banner, /causeResolutionCovers \|\| causeResolutions\.some\(Boolean\)/);
  assert.match(banner, /authStatus === "anonymous" && causeControlsPresent/);
  // One owner holds resolved state, as with the pattern scope.
  assert.doesNotMatch(cause, /useResolved/);
});

test("reopening a pattern never falls back to a resolution the server would refuse", () => {
  const actions = source("src/components/FailureActions.tsx");
  const banner = source("src/components/PatternBanner.tsx");

  // Reopen is gated on resolvable, but starting a NEW resolution additionally
  // needs canResolve, so a resolved pattern that no longer qualifies for a
  // fresh resolution can be reopened without ever offering Resolve.
  assert.match(actions, /\{resolvable && \(isResolved \?/);
  assert.match(actions, /\) : canResolve && \(/);
  // A resolution write outlives the pattern it started on, so a late response
  // must not land on whichever failure the user navigated to.
  assert.match(actions, /const startedFailureID = failureID;/);
  assert.match(actions, /if \(activeFailureID\.current !== startedFailureID\) return;/);
  assert.match(actions, /open=\{resolvable && canResolve && resolveOpen\}/);
  // One owner holds resolved state. Two independent useResolved() copies could
  // diverge and pair a "Resolved" chip with a "Resolve pattern" button.
  assert.doesNotMatch(actions, /useResolved/);
  assert.match(banner, /isResolved=\{Boolean\(resolvedEntry\)\}/);
  assert.match(banner, /onResolvedChange=\{refetchResolved\}/);

  // The read that follows a resolution write keeps prior state and retries, so
  // a transient failure cannot leave a stale control until remount.
  const data = source("src/hooks/useData.ts");
  assert.match(data, /if \(r\.status === 404\) return emptyResolved\(\);/);
  assert.match(data, /if \(!r\.ok\) throw new Error\(`resolved\.json: \$\{r\.status\}`\);/);
  assert.match(data, /timer = window\.setTimeout\(load, Math\.min\(8000/);
  assert.match(data, /const resolvedReadAttempts = \d+;/);
  // causes is omitted when empty, so it is filled in before any consumer reads
  // it and none of them has to guard the lookup.
  assert.match(data, /causes: d\.causes \?\? \{\}/);
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

test("a cause opens the build its displayed suggestion came from", () => {
  const otherBuild: BuildResult = { ...groundedRun, build_id: "208726" };
  const runs = [groundedRun, otherBuild];

  // Without a reported remediation the first eligible member still wins.
  assert.deepEqual(causalGroupFixTarget(firstGroup, runs), { buildID: "208060", testName: "fails" });

  // With one, the button opens the same analysis the briefing quoted, so the
  // two surfaces cannot disagree about which build the fix came from.
  assert.deepEqual(
    causalGroupFixTarget(
      { ...firstGroup, remediation: { suggested_fix: "Raise the budget.", build_id: "208726" } },
      runs,
    ),
    { buildID: "208726", testName: "fails" },
  );

  // A suggestion from a build that cannot start a Fix investigation falls back
  // rather than leaving the cause with no route at all.
  assert.deepEqual(
    causalGroupFixTarget(
      { ...firstGroup, remediation: { suggested_fix: "Raise the budget.", build_id: "208726" } },
      [groundedRun, { ...otherBuild, test_cases: [] }],
    ),
    { buildID: "208060", testName: "fails" },
  );
});

// A cause offers no fix target for two unrelated reasons, and only one of them
// is about eligibility. Reporting the wrong one sends the reader looking for a
// JUnit problem that is not there.
test("a cause separates builds that left the window from builds with no eligible failure", () => {
  // Builds present, but the failure carries no analysis, so nothing qualifies.
  assert.equal(causalGroupEvidencePresent(firstGroup, [junitRun]), true);
  assert.equal(causalGroupFixTarget(firstGroup, [junitRun]), null);

  // The window has rolled past every build this cause names.
  const laterRun: BuildResult = { ...groundedRun, build_id: "999999" };
  assert.equal(causalGroupEvidencePresent(firstGroup, [laterRun]), false);
  assert.equal(causalGroupFixTarget(firstGroup, [laterRun]), null);

  // A readable build still resolves a target.
  assert.deepEqual(causalGroupFixTarget(firstGroup, [groundedRun]), {
    buildID: groundedRun.build_id,
    testName: "fails",
  });
});
