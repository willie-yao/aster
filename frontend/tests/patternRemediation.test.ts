import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  causalRemediationBlockedReason,
  notInvestigatedReason,
  patternRemediationPresentation,
  remediationUnavailableReason,
  unhashedRemediationReason,
  unrecurringPatternRemediationReason,
} from "../src/lib/patternRemediation.js";
import type {
  PatternCausalGroup,
  PatternRemediationInvestigationState,
} from "../src/types/dashboard.js";

const states: PatternRemediationInvestigationState[] = [
  "not_investigated",
  "queued",
  "investigating",
  "verifying",
  "actionable",
  "already_fixed",
  "external_dependency",
  "environment_or_infrastructure",
  "mitigation_only",
  "insufficient_evidence",
  "failed",
  "stale",
];

const repeatedGroup: PatternCausalGroup = {
  id: "group-id",
  content_hash: "group-hash",
  builds: ["208060", "208726"],
  root_cause: "first cause",
  confidence: "high",
};

test("causal remediation defaults to an explicit non-actionable state", () => {
  assert.deepEqual(patternRemediationPresentation(), {
    state: "not_investigated",
    label: "Not investigated",
    message: notInvestigatedReason,
    detail: undefined,
  });
});

test("every remediation investigation state has user-visible copy", () => {
  for (const state of states) {
    const presentation = patternRemediationPresentation({
      causal_group_id: "group",
      causal_group_hash: "hash",
      state,
    });
    assert.equal(presentation.state, state);
    assert.ok(presentation.label);
    assert.ok(presentation.message);
  }
});

test("a server reason is carried separately so the specific account can lead", () => {
  const presentation = patternRemediationPresentation({
    causal_group_id: "group",
    causal_group_hash: "hash",
    state: "failed",
    reason: "The pinned source revision was unavailable.",
  });
  // The state's own copy stays available as the fallback, and the server's
  // account of this particular failure is what the card shows.
  assert.match(presentation.message, /Published causal analysis is unchanged/);
  assert.equal(presentation.detail, "The pinned source revision was unavailable.");

  // A reason that only repeats the state copy is not a second sentence.
  const generic = patternRemediationPresentation({
    causal_group_id: "group",
    causal_group_hash: "hash",
    state: "failed",
    reason: presentation.message,
  });
  assert.equal(generic.detail, undefined);

  const card = readFileSync(resolve(process.cwd(), "src/components/CausalGroupRemediation.tsx"), "utf8");
  // The visible line prefers the specific reason; the disclosure no longer
  // repeats it, so a distinct failure account cannot end up collapsed.
  assert.match(card, /presentation\.detail \?\? presentation\.message/);
  assert.doesNotMatch(card, /investigation\.reason !== conciseMessage/);
});

test("an unreachable investigation reports why instead of looking pending", () => {
  assert.deepEqual(causalRemediationBlockedReason(repeatedGroup, undefined, false), {
    scope: "deployment",
    label: "Unavailable on this deployment",
    message: remediationUnavailableReason,
  });
  assert.equal(causalRemediationBlockedReason(repeatedGroup, undefined, true), null);
});

test("a single-build cause is investigable, gated only by the deployment", () => {
  const singleBuild: PatternCausalGroup = { ...repeatedGroup, builds: ["209114"] };
  assert.equal(causalRemediationBlockedReason(singleBuild, undefined, true), null);
  assert.deepEqual(causalRemediationBlockedReason(singleBuild, undefined, false), {
    scope: "deployment",
    label: "Unavailable on this deployment",
    message: remediationUnavailableReason,
  });
});

test("blocked reasons report the permanent condition before the deployment one", () => {
  const unhashed: PatternCausalGroup = {
    builds: ["208060", "208726"],
    root_cause: "first cause",
    confidence: "high",
  };
  assert.deepEqual(causalRemediationBlockedReason(unhashed, undefined, false), {
    scope: "cause",
    label: "Not addressable",
    message: unhashedRemediationReason,
  });
});

test("a published verdict outlives the capability that produced it", () => {
  assert.equal(
    causalRemediationBlockedReason(
      repeatedGroup,
      { causal_group_id: "group-id", causal_group_hash: "group-hash", state: "actionable" },
      false,
    ),
    null,
  );
});

test("a disabled capability reports every unresolvable state as unavailable", () => {
  const unresolvable: PatternRemediationInvestigationState[] = [
    "not_investigated",
    "queued",
    "investigating",
    "verifying",
  ];

  // Without the operation these can never advance, and the component neither
  // polls nor offers a control, so reporting them verbatim strands the card.
  for (const state of unresolvable) {
    assert.deepEqual(
      causalRemediationBlockedReason(
        repeatedGroup,
        { causal_group_id: "group-id", causal_group_hash: "group-hash", state },
        false,
      ),
      { scope: "deployment", label: "Unavailable on this deployment", message: remediationUnavailableReason },
      state,
    );
  }

  for (const state of states.filter((candidate) => !unresolvable.includes(candidate))) {
    assert.equal(
      causalRemediationBlockedReason(
        repeatedGroup,
        { causal_group_id: "group-id", causal_group_hash: "group-hash", state },
        false,
      ),
      null,
      state,
    );
  }
});

test("an unrecognized state is blocked exactly like the default it renders as", () => {
  assert.deepEqual(
    causalRemediationBlockedReason(
      repeatedGroup,
      {
        causal_group_id: "group-id",
        causal_group_hash: "group-hash",
        state: "from_a_newer_engine" as PatternRemediationInvestigationState,
      },
      false,
    ),
    { scope: "deployment", label: "Unavailable on this deployment", message: remediationUnavailableReason },
  );
});

test("causal remediation renders per cause and keeps normal actions blocked", () => {
  const component = readFileSync(resolve(process.cwd(), "src/components/CausalGroupRemediation.tsx"), "utf8");
  const banner = readFileSync(resolve(process.cwd(), "src/components/PatternBanner.tsx"), "utf8");

  // The row names the mechanism it reports. A generic "Remediation" label made
  // a block on this one operation read as if nothing could be done at all.
  assert.match(component, />\s*Verified fix investigation\s*</);
  assert.doesNotMatch(component, />\s*Remediation\s*</);
  assert.match(component, /aria-live="polite"/);
  assert.match(component, /Investigation details/);
  assert.match(component, /Investigate possible fix/);
  assert.match(component, /causal_remediation_investigation/);
  assert.match(component, /Preview Fix PR/);
  assert.match(component, /causal_remediation_fix_preview/);
  assert.match(component, /No GitHub PR will be created/);
  assert.doesNotMatch(component, /Create PR|Confirm PR/);

  // The blocked verdict replaces the control instead of sitting beside it.
  assert.match(component, /const canStart = !blocked &&/);
  assert.match(component, /const canPreview = !blocked &&/);

  // A blocked cause never polls the operation and discloses nothing. Build
  // count no longer gates this: a cause seen once is investigable too.
  assert.match(component, /const addressable = Boolean\(operationRef\)/);
  assert.match(component, /const pollable = !blocked && addressable && operationAvailable && authStatus === "authenticated"/);
  assert.match(component, /const details = blocked \? undefined :/);

  // A missing deployment capability and a per-cause verdict mean unrelated
  // things, so they must not render as the same chip.
  assert.match(component, /const capabilityBlocked = blocked\?\.scope === "deployment"/);
  assert.match(component, /icon=\{capabilityBlocked \? <CloudOff aria-hidden \/> : undefined\}/);
  assert.match(component, /variant=\{capabilityBlocked \? "filled" : "outlined"\}/);

  // Causes now carry their own h4 heading, so the remediation label sits one
  // level below it rather than competing with it.
  assert.match(component, /component="h5"[\s\S]*>\s*Verified fix investigation\s*</);

  // Remediation is decided per cause, so it renders inside the causal group card.
  assert.match(banner, /causalGroups\.map\(\(group, index\)[\s\S]*<CausalGroupRemediation/);
  assert.equal(banner.match(/<CausalGroupRemediation/g)?.length, 1);
  assert.doesNotMatch(banner, /<PatternRemediation/);

  // The card is keyed by group identity, so a refreshed group cannot inherit a
  // previous group's in-flight status, preview, or idempotency key.
  assert.match(banner, /key=\{`\$\{group\.id \?\? ""\}:\$\{group\.content_hash \?\? ""\}:/);
});

test("a cause in an unclassified pattern is reported instead of offered", () => {
  // The resolver runs the investigation only on a recurring, systemic pattern,
  // so a cause inside an unclassified one must not be offered a control the
  // server would reject.
  assert.deepEqual(causalRemediationBlockedReason(repeatedGroup, undefined, true, false), {
    scope: "cause",
    label: "Not eligible",
    message: unrecurringPatternRemediationReason,
  });
  // Pattern eligibility defaults to true so existing callers are unaffected.
  assert.equal(causalRemediationBlockedReason(repeatedGroup, undefined, true), null);
  assert.equal(causalRemediationBlockedReason(repeatedGroup, undefined, true, true), null);
});

test("a blocked investigation names the path that stays open", () => {
  const component = readFileSync(resolve(process.cwd(), "src/components/CausalGroupRemediation.tsx"), "utf8");
  const banner = readFileSync(resolve(process.cwd(), "src/components/PatternBanner.tsx"), "utf8");

  // The row reports one mechanism, so a block on it is not the end of the road.
  assert.match(component, /blocked && chatAvailable/);
  assert.match(component, /ask about this cause in the pattern chat below/);
  // The chat picks its own evidence builds, so the copy must not claim parity.
  assert.doesNotMatch(component, /reads the same evidence/);
  // Only promised where a chat session can actually run.
  assert.match(banner, /chatAvailable=\{Boolean\(chatRef\)\}/);
});
