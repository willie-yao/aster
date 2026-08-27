import assert from "node:assert/strict";
import { test } from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import { externalCause, patternExternalCause } from "../src/lib/patternFixGuidance.js";
import type { CausalGroupFixTarget } from "../src/lib/patternFixGuidance.js";
import type {
  AnalysisCauseLocation,
  PatternAnalysis,
  PatternCausalGroup,
} from "../src/types/dashboard.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { CausalGroupFixNotice, CausalGroupFixButton } = (await vite.ssrLoadModule("/src/components/CausalGroupFixRouting.tsx")) as {
  CausalGroupFixNotice: (props: {
    jobID?: string;
    target: CausalGroupFixTarget | null;
    externalCause?: AnalysisCauseLocation | null;
    evidencePresent?: boolean;
  }) => ReturnType<typeof createElement>;
  CausalGroupFixButton: (props: {
    jobID?: string;
    target: CausalGroupFixTarget | null;
    showBuild?: boolean;
    stale?: boolean;
  }) => ReturnType<typeof createElement>;
};

// The notice and the action are rendered in two places on a cause card: the
// prose stays in the body while the button moves to the action bar. They are
// exercised together here because the invariants under test are about what a
// reader sees for one cause, not about which half renders it.
function CausalGroupFixRouting(props: {
  jobID?: string;
  target: CausalGroupFixTarget | null;
  externalCause?: AnalysisCauseLocation | null;
  showBuild?: boolean;
  stale?: boolean;
  evidencePresent?: boolean;
}) {
  return createElement(
    "div",
    null,
    createElement(CausalGroupFixNotice, {
      jobID: props.jobID,
      target: props.target,
      externalCause: props.externalCause,
      evidencePresent: props.evidencePresent,
    }),
    createElement(CausalGroupFixButton, {
      jobID: props.jobID,
      target: props.target,
      showBuild: props.showBuild,
      stale: props.stale,
    }),
  );
}
const { PatternFixGuidance } = (await vite.ssrLoadModule("/src/components/PatternFixGuidance.tsx")) as {
  PatternFixGuidance: (props: {
    jobID: string;
    buildID: string;
    externalCause?: AnalysisCauseLocation | null;
    chatAvailable?: boolean;
  }) => ReturnType<typeof createElement>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
await vite.close();

function render(element: ReturnType<typeof createElement>): string {
  return renderToStaticMarkup(
    createElement(ThemeProvider, { theme: defaultTheme }, createElement(MemoryRouter, null, element)),
  );
}

// The independently confirmed case from the report: the cause of the DRA
// conformance failure lives in kubernetes/kubernetes, and a maintainer fixed it
// there in kubernetes/kubernetes#141426.
const draCause: AnalysisCauseLocation = {
  repository: "kubernetes/kubernetes",
  external: true,
  files: ["pkg/kubelet/cm/devicemanager/manager.go"],
};

const projectCause: AnalysisCauseLocation = {
  repository: "kubernetes-sigs/cluster-api-provider-azure",
};

function pattern(groups: PatternCausalGroup[]): PatternAnalysis {
  return {
    subject: "several failures",
    generated_at: "2026-08-14T00:00:00Z",
    builds_analyzed: groups.reduce((total, group) => total + group.builds.length, 0),
    recurrence_classification: "shared_cause",
    causal_groups: groups,
    systemic: true,
    confidence: "high",
    summary: "Several causes recur.",
  };
}

function group(overrides: Partial<PatternCausalGroup> = {}): PatternCausalGroup {
  return { builds: ["1", "2"], root_cause: "cause", confidence: "high", ...overrides };
}

test("only a dependency cause is treated as upstream", () => {
  assert.deepEqual(externalCause(draCause), draCause);
  assert.equal(externalCause(projectCause), null);
  assert.equal(externalCause(undefined), null);
  // A repository is required: "external" alone names nothing actionable.
  assert.equal(externalCause({ repository: "", external: true }), null);
});

test("a pattern reports an upstream cause only when every cause agrees", () => {
  assert.deepEqual(patternExternalCause(pattern([group({ cause_location: draCause })])), draCause);
  assert.deepEqual(
    patternExternalCause(pattern([group({ cause_location: draCause }), group({ cause_location: draCause })])),
    draCause,
  );
  assert.equal(
    patternExternalCause(pattern([group({ cause_location: draCause }), group({ cause_location: projectCause })])),
    null,
  );
  assert.equal(
    patternExternalCause(
      pattern([
        group({ cause_location: draCause }),
        group({ cause_location: { repository: "kubernetes-sigs/cluster-api", external: true } }),
      ]),
    ),
    null,
  );
  assert.equal(patternExternalCause(pattern([group({ cause_location: draCause }), group()])), null);
  assert.equal(patternExternalCause(pattern([])), null);
  // Repository comparison matches the backend merge, which is case-insensitive.
  assert.deepEqual(
    patternExternalCause(
      pattern([
        group({ cause_location: draCause }),
        group({ cause_location: { repository: "Kubernetes/Kubernetes", external: true } }),
      ]),
    ),
    draCause,
  );
});

test("a cause with no fix target names the dependency instead of a dead end", () => {
  const html = render(
    createElement(CausalGroupFixRouting, { jobID: "job", target: null, externalCause: draCause }),
  );

  assert.match(html, /kubernetes\/kubernetes/);
  assert.match(html, /pkg\/kubelet\/cm\/devicemanager\/manager\.go/);
  assert.match(html, /href="https:\/\/github\.com\/kubernetes\/kubernetes"/);
  assert.match(html, /unverified/);
  assert.match(html, /does not open\s+pull requests in a dependency/);
  assert.match(html, /project-side mitigation/);
  assert.doesNotMatch(html, /meets the Fix eligibility requirements/);
});

test("a cause with no upstream owner keeps the existing generic message", () => {
  const html = render(createElement(CausalGroupFixRouting, { jobID: "job", target: null, externalCause: null }));

  assert.match(html, /No failed JUnit test in these builds meets the Fix eligibility requirements/);
  assert.doesNotMatch(html, /dependency/);
});

test("the pattern panel names the dependency instead of reporting unavailability", () => {
  const upstream = render(
    createElement(PatternFixGuidance, { jobID: "job", buildID: "208060", externalCause: draCause }),
  );
  assert.match(upstream, /Cause is in a dependency/);
  assert.match(upstream, /kubernetes\/kubernetes/);
  assert.match(upstream, /pkg\/kubelet\/cm\/devicemanager\/manager\.go/);
  assert.doesNotMatch(upstream, /Fix proposal unavailable/);
  // The evidence route stays available so the reader can confirm the diagnosis.
  assert.match(upstream, /View failed tests/);

  const generic = render(createElement(PatternFixGuidance, { jobID: "job", buildID: "208060" }));
  assert.match(generic, /Fix proposal unavailable/);
  assert.match(generic, /No failed JUnit test in the affected builds meets the Fix eligibility requirements/);
  assert.doesNotMatch(generic, /dependency/);
});

const fixTarget: CausalGroupFixTarget = { buildID: "208060", testName: "fails" };

test("a cause owned by a dependency reports it even when a Fix route exists", () => {
  // Ownership used to be suppressed whenever a Fix button rendered, which made
  // an upstream cause indistinguishable from one the project can actually fix.
  const html = render(
    createElement(CausalGroupFixRouting, {
      jobID: "job",
      target: fixTarget,
      externalCause: draCause,
    }),
  );

  assert.match(html, /kubernetes\/kubernetes/);
  assert.match(html, /project-side mitigation/);
  // The action itself is unchanged and still opens the failing test.
  assert.match(html, /aria-label="fails in build 208060, open representative failure"/i);

  const owned = render(
    createElement(CausalGroupFixRouting, {
      jobID: "job",
      target: fixTarget,
      externalCause: null,
    }),
  );
  assert.match(owned, /aria-label="fails in build 208060, open representative failure"/i);
  assert.doesNotMatch(owned, /dependency/);
});

test("stale causes point to evidence without offering it as the live route", () => {
  const live = render(createElement(CausalGroupFixRouting, {
    jobID: "job",
    target: fixTarget,
    externalCause: null,
  }));
  const html = render(createElement(CausalGroupFixRouting, {
    jobID: "job",
    target: fixTarget,
    externalCause: null,
    stale: true,
  }));

  // The button navigates either way, so the wording does not change. Both read
  // as links now, so the demotion is carried by the underline, which still has
  // to actually differ.
  assert.match(html, /aria-label="fails in build 208060, open representative failure"/i);
  assert.match(live, /MuiButton-text/);
  assert.match(html, /MuiButton-text/);
  assert.match(live, /text-underline-offset:3px/);
  assert.doesNotMatch(html, /text-underline-offset:3px/);
});

test("the pattern panel only points at a chat that is on the page", () => {
  // Pattern chat needs a systemic pattern, which this panel does not, so the
  // panel could previously send the reader to a chat that never rendered.
  const withChat = render(
    createElement(PatternFixGuidance, { jobID: "job", buildID: "208060", chatAvailable: true }),
  );
  assert.match(withChat, /The pattern chat below/);
  assert.match(withChat, /A fix proposal becomes available/);

  const withoutChat = render(createElement(PatternFixGuidance, { jobID: "job", buildID: "208060" }));
  assert.doesNotMatch(withoutChat, /pattern chat/);
  // The guidance that does not depend on chat survives.
  assert.match(withoutChat, /A fix proposal becomes available/);
  assert.match(withoutChat, /View failed tests/);
});

// The two dead ends read the same before this split, so a cause whose builds had
// aged out was reported as an eligibility problem in tests that were never
// examined.
test("a cause without a fix target names which dead end it hit", () => {
  const eligibility = render(
    createElement(CausalGroupFixRouting, { jobID: "job", target: null, evidencePresent: true }),
  );
  assert.match(eligibility, /Fix eligibility requirements/);
  assert.doesNotMatch(eligibility, /left the analysis window/);

  const expired = render(
    createElement(CausalGroupFixRouting, { jobID: "job", target: null, evidencePresent: false }),
  );
  assert.match(expired, /have left the analysis window/);
  assert.doesNotMatch(expired, /Fix eligibility requirements/);

  // An upstream cause still reports ownership rather than either dead end.
  const upstream = render(
    createElement(CausalGroupFixRouting, {
      jobID: "job", target: null, externalCause: draCause, evidencePresent: false,
    }),
  );
  assert.doesNotMatch(upstream, /left the analysis window/);
  assert.match(upstream, /kubernetes\/kubernetes/);
});
