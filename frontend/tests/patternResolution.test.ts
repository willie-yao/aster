import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve as resolvePath } from "node:path";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import {
  patternFullyResolved,
  unlistedCauseResolutions,
  unlistedPatternResolutions,
} from "../src/lib/dashboardOverview.js";
import type { BuildResult, FlakinessReport, PatternAnalysis, PatternRefreshStatus, ResolvedEntry, ResolvedState } from "../src/types/dashboard.js";
import type { AuthState } from "../src/hooks/useAuth.js";
import type { Capabilities } from "../src/types/capabilities.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { PatternBanner } = (await vite.ssrLoadModule("/src/components/PatternBanner.tsx")) as {
  PatternBanner: (props: {
    pattern: PatternAnalysis;
    jobID?: string;
    refreshStatus?: PatternRefreshStatus;
    runs?: BuildResult[];
  }) => ReturnType<typeof createElement>;
};
const { UnlistedResolutionRow } = (await vite.ssrLoadModule("/src/components/NeedsAttention.tsx")) as {
  UnlistedResolutionRow: (props: {
    scope: "pattern" | "cause";
    id: string;
    entry: ResolvedEntry;
    filePrefix: string;
    onRestored: () => void;
  }) => ReturnType<typeof createElement>;
};
const { CapabilitiesContext } = (await vite.ssrLoadModule("/src/hooks/useCapabilities.ts")) as {
  CapabilitiesContext: React.Context<Capabilities>;
};
const { AuthContext } = (await vite.ssrLoadModule("/src/hooks/useAuth.ts")) as {
  AuthContext: React.Context<AuthState>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
await vite.close();

function source(file: string): string {
  return readFileSync(resolvePath(process.cwd(), file), "utf8");
}

const serverCapabilities: Capabilities = {
  mode: "server",
  features: { actions: true, action_requests: true, action_eligibility: true, fix_prs: true },
  auth: { mode: "oauth" },
};

const admin: AuthState = {
  status: "authenticated",
  login: "willie-yao",
  mode: "oauth",
  signIn: () => {},
  signOut: async () => {},
};

// causalGroupPattern mirrors what the engine publishes today: every recurring
// pattern carries a recurrence_classification and causal groups.
function causalGroupPattern(overrides: Partial<PatternAnalysis> = {}): PatternAnalysis {
  return {
    id: "pattern-1",
    subject: "periodic-capz-e2e-main",
    job_id: "periodic-capz-e2e-main",
    generated_at: "2026-08-18T00:00:00Z",
    builds_analyzed: 3,
    recurrence_classification: "shared_cause",
    causal_groups: [{ builds: ["100", "250"], root_cause: "etcd leader election times out", confidence: "high" }],
    shared_builds: ["100", "250"],
    systemic: true,
    confidence: "high",
    shared_root_cause: "etcd leader election times out",
    summary: "Two builds fail on the same etcd timeout.",
    ...overrides,
  };
}

function render(
  pattern: PatternAnalysis,
  auth: AuthState = admin,
  capabilities = serverCapabilities,
  refreshStatus?: PatternRefreshStatus,
  runs?: BuildResult[],
): string {
  const tree: ReactNode = createElement(
    ThemeProvider,
    { theme: defaultTheme },
    createElement(
      MemoryRouter,
      null,
      createElement(
        CapabilitiesContext.Provider,
        { value: capabilities },
        createElement(
          AuthContext.Provider,
          { value: auth },
          createElement(PatternBanner, { pattern, jobID: pattern.job_id, refreshStatus, runs }),
        ),
      ),
    ),
  );
  return renderToStaticMarkup(tree);
}

test("an admin can dismiss the causal-group patterns the engine actually publishes", () => {
  const html = render(causalGroupPattern());

  // The regression: analysisOnly was true for every published pattern, so the
  // control never rendered anywhere.
  assert.match(html, /Resolve pattern/);
  // Pattern-level drafting stays blocked for causal-group results, and the
  // pattern-level eligibility notice stays out of the way.
  assert.doesNotMatch(html, /Draft issue/);
  assert.doesNotMatch(html, /Draft fix PR/);
  assert.doesNotMatch(html, /Preview generation failed/);
});

test("dismissal follows the same gates the server enforces", () => {
  assert.doesNotMatch(render(causalGroupPattern({ systemic: false })), /Resolve pattern/);
  assert.doesNotMatch(render(causalGroupPattern({ id: undefined })), /Resolve pattern/);
  assert.doesNotMatch(
    render(causalGroupPattern({ lifecycle: { state: "verified_fixed", reason: "verified fixed" } })),
    /Resolve pattern/,
  );
  // The server derives the recurrence watermark from shared_builds.
  assert.doesNotMatch(render(causalGroupPattern({ shared_builds: [] })), /Resolve pattern/);
  // findPattern rejects a stale refresh or evidence that left the job window.
  assert.match(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "current", evidence_available: true }),
    /Resolve pattern/,
  );
  assert.doesNotMatch(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "current", evidence_available: false }),
    /Resolve pattern/,
  );
  // A retained correlation still describes the published pattern, so dismissal
  // stays available while its evidence is readable.
  assert.match(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "retained", evidence_available: true }),
    /Resolve pattern/,
  );
  assert.doesNotMatch(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "retained", evidence_available: false }),
    /Resolve pattern/,
  );
});

test("dismissal stays behind admin auth and the actions capability", () => {
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  assert.doesNotMatch(render(causalGroupPattern(), anonymous), /Resolve pattern/);

  const readOnly: Capabilities = { mode: "static", features: { actions: false } };
  assert.doesNotMatch(render(causalGroupPattern(), admin, readOnly), /Resolve pattern/);
});

test("the pattern-level eligibility notice stays with the legacy contract", () => {
  // A legacy pattern with no remediation targets gets a deterministic blocked
  // hint, so the drafting notice renders and resolution is still offered.
  const legacy = causalGroupPattern({ recurrence_classification: undefined, causal_groups: undefined });
  const legacyHtml = render(legacy);
  assert.match(legacyHtml, /Preview generation failed/);
  assert.match(legacyHtml, /Resolve pattern/);

  // The same notice is suppressed on a causal-group result, where pattern-level
  // drafting does not apply and per-cause remediation is shown instead.
  assert.doesNotMatch(render(causalGroupPattern()), /Preview generation failed/);
});

// Acknowledging one cause must not hide its siblings, so a signed cause carries
// its own control and the pattern-level one stands down rather than offering a
// second acknowledgement with a wider blast radius.
test("a signed cause is resolved on its own and the pattern-level control stands down", () => {
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
        { builds: ["250"], root_cause: "etcd timeout", confidence: "high", signature: "sig-b" },
      ],
    }),
  );

  assert.match(html, /Resolve failure/);
  assert.doesNotMatch(html, /Resolve pattern/);
});

// A group the engine could not sign has no durable key to record a resolution
// under, so the pattern-level acknowledgement has to stay reachable.
test("an unsigned cause keeps the pattern-level control as the fallback", () => {
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
        { builds: ["250"], root_cause: "etcd timeout", confidence: "high" },
      ],
    }),
  );

  // The signed cause still gets its own control; the unsigned one gets none.
  assert.match(html, /Resolve failure/);
  assert.match(html, /Resolve pattern/);
});

// Per-cause resolution keys on the same server gates the pattern scope uses, so
// a cause the server would refuse must not be offered one.
test("per-cause resolution follows the gates the server enforces", () => {
  const signed = (overrides: Partial<PatternAnalysis> = {}) =>
    render(
      causalGroupPattern({
        causal_groups: [{ builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" }],
        ...overrides,
      }),
    );

  assert.match(signed(), /Resolve failure/);
  assert.doesNotMatch(signed({ systemic: false }), /Resolve failure/);
  assert.doesNotMatch(
    signed({ lifecycle: { state: "verified_fixed", reason: "verified fixed" } }),
    /Resolve failure/,
  );
  // resolve.CauseWatermark parses build ids as decimal integers.
  assert.doesNotMatch(
    render(
      causalGroupPattern({
        causal_groups: [{ builds: ["not-a-build"], root_cause: "x", confidence: "high", signature: "sig-a" }],
      }),
    ),
    /Resolve failure/,
  );
});

test("per-cause resolution stays behind admin auth and the actions capability", () => {
  const withSignedCause = causalGroupPattern({
    causal_groups: [{ builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" }],
  });
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  const readOnly: Capabilities = { mode: "static", features: { actions: false } };

  assert.doesNotMatch(render(withSignedCause, anonymous), /Resolve failure/);
  assert.doesNotMatch(render(withSignedCause, admin, readOnly), /Resolve failure/);
  // An anonymous viewer still needs a way in, so the pattern-level block stays
  // mounted for its sign-in prompt even though it offers no resolution.
  assert.match(render(withSignedCause, anonymous), /Sign in to manage this failure/);
});

// A dismissal whose pattern has left the active recurring set is retained by
// the fetcher but disappears from the overview, so the dismissed-patterns
// disclosure lists it with the restore path.

function reportWith(ids: string[], signature?: string): FlakinessReport {
  return {
    generated_at: "2026-08-18T00:00:00Z",
    most_flaky: [],
    persistent_failures: [],
    recently_broken: [],
    build_failures: [],
    recurring_patterns: ids.map((id) =>
      causalGroupPattern({
        id,
        causal_groups: [
          { builds: ["100", "250"], root_cause: "etcd leader election times out", confidence: "high", signature },
        ],
      }),
    ),
  };
}

function resolution(overrides: Partial<ResolvedEntry> = {}): ResolvedEntry {
  return {
    resolved_at: "2026-08-17T00:00:00Z",
    resolved_by: "willie-yao",
    watermark: "250",
    subject: "periodic-capz-e2e-main",
    ...overrides,
  };
}

function resolvedState(overrides: Partial<ResolvedState> = {}): ResolvedState {
  return { resolved: {}, causes: {}, ...overrides };
}

// recurring_patterns comes from patterns.CollectRecurring, which publishes only
// systemic, lifecycle-active patterns and does not truncate. So an id missing
// from it is either a pattern that aged out or one whose lifecycle moved on,
// and the overview, which reads nothing else, cannot tell them apart.
test("only pattern resolutions absent from the active recurring set are listed as unlisted", () => {
  const report = reportWith(["pattern-1"]);
  const state = resolvedState({
    resolved: { "pattern-1": resolution(), "pattern-gone": resolution() },
  });

  assert.deepEqual(
    unlistedPatternResolutions(report, state, true).map(([id]) => id),
    ["pattern-gone"],
  );
});

test("unlisted pattern resolutions are withheld where they could not be reopened", () => {
  const state = resolvedState({ resolved: { "pattern-gone": resolution() } });

  // An unread report cannot distinguish an unlisted pattern from an unread one.
  assert.deepEqual(unlistedPatternResolutions(null, state, true), []);
  // Reopening is the only thing a viewer can do with one, so a read-only or
  // signed-out viewer is shown nothing rather than a row that cannot act.
  assert.deepEqual(unlistedPatternResolutions(reportWith([]), state, false), []);
});

// A cause leaves the published set when its builds age out, and its resolution
// is retained for the same reason a pattern's is, so the overview has to offer
// the only remaining reopen path.
test("only cause resolutions no published pattern still shows are listed as unlisted", () => {
  const report = reportWith(["pattern-1"], "etcd-sig");
  const state = resolvedState({
    causes: { "etcd-sig": resolution(), "gone-sig": resolution() },
  });

  assert.deepEqual(
    unlistedCauseResolutions(report, state, true).map(([signature]) => signature),
    ["gone-sig"],
  );
  assert.deepEqual(unlistedCauseResolutions(null, state, true), []);
  assert.deepEqual(unlistedCauseResolutions(report, state, false), []);
});

// The overview hides a pattern only once every failure it represents has been
// acknowledged, so resolving one cause of two leaves the other one visible.
test("a pattern counts as resolved only when every cause is", () => {
  const twoCauses = causalGroupPattern({
    causal_groups: [
      { builds: ["100"], root_cause: "a", confidence: "high", signature: "sig-a" },
      { builds: ["250"], root_cause: "b", confidence: "high", signature: "sig-b" },
    ],
  });

  assert.equal(patternFullyResolved(twoCauses, resolvedState()), false);
  assert.equal(
    patternFullyResolved(twoCauses, resolvedState({ causes: { "sig-a": resolution() } })),
    false,
  );
  assert.equal(
    patternFullyResolved(
      twoCauses,
      resolvedState({ causes: { "sig-a": resolution(), "sig-b": resolution() } }),
    ),
    true,
  );
  // A pattern-scope resolution covers every cause at once.
  assert.equal(
    patternFullyResolved(twoCauses, resolvedState({ resolved: { "pattern-1": resolution() } })),
    true,
  );
  // An unsigned cause can never be resolved on its own, so the pattern can only
  // reach the resolved list through the pattern scope.
  const unsigned = causalGroupPattern({
    causal_groups: [{ builds: ["100"], root_cause: "a", confidence: "high" }],
  });
  assert.equal(patternFullyResolved(unsigned, resolvedState({ causes: { "": resolution() } })), false);
  // A pattern with no causes at all is pattern-scope only.
  const noCauses = causalGroupPattern({ causal_groups: [] });
  assert.equal(patternFullyResolved(noCauses, resolvedState()), false);
});

function renderUnlisted(
  entry: ResolvedEntry,
  auth: AuthState = admin,
  capabilities = serverCapabilities,
  scope: "pattern" | "cause" = "pattern",
): string {
  const tree: ReactNode = createElement(
    ThemeProvider,
    { theme: defaultTheme },
    createElement(
      MemoryRouter,
      null,
      createElement(
        CapabilitiesContext.Provider,
        { value: capabilities },
        createElement(
          AuthContext.Provider,
          { value: auth },
          createElement(UnlistedResolutionRow, {
            scope,
            id: scope === "cause" ? "gone-sig" : "pattern-gone",
            entry,
            filePrefix: "",
            onRestored: () => {},
          }),
        ),
      ),
    ),
  );
  return renderToStaticMarkup(tree);
}

test("an unlisted pattern resolution offers Reopen and explains why it has no analysis", () => {
  const html = renderUnlisted(resolution({ note: "fixed upstream in kubernetes/kubernetes#1" }));

  assert.match(html, /Reopen pattern/);
  assert.match(html, /Resolved by willie-yao/);
  assert.match(html, /Its pattern is no longer among the active recurring failures/);
  assert.match(html, /fixed upstream/);
  // The pattern left the active set, so there is nothing to resolve again and
  // no pattern-level analysis to draft from.
  assert.doesNotMatch(html, /Resolve pattern/);
  assert.doesNotMatch(html, /Draft issue/);
  // resolved.json records no job and the overview cannot confirm the pattern is
  // still published anywhere, so the row has no destination to offer. That also
  // keeps the Reopen button out of a surrounding link.
  assert.doesNotMatch(html, /<a /);
});

// One job can carry several resolved causes, so the row names the cause rather
// than leaving them all to render as the same job.
test("an unlisted cause resolution offers Reopen and names the cause it resolved", () => {
  const html = renderUnlisted(
    resolution({ cause: "etcd leader election times out", note: "fixed upstream" }),
    admin,
    serverCapabilities,
    "cause",
  );

  assert.match(html, /Reopen failure/);
  assert.match(html, /Its cause is no longer among the active recurring failures/);
  assert.match(html, /etcd leader election times out/);
  assert.match(html, /fixed upstream/);
  // Reopening a cause must never offer a fresh resolution for it.
  assert.doesNotMatch(html, /Resolve failure/);
  assert.doesNotMatch(html, /<a /);

  // The overview row is a flex row that neither wraps nor stacks, so the
  // control owns a column here: a failed reopen would otherwise put its alert
  // beside the button and crush it.
  const control = source("src/components/CauseResolution.tsx");
  assert.match(control, /const Wrapper = bar \? Fragment : InlineWrapper/);
  assert.match(control, /flexDirection: "column"/);
});

test("reopening an unlisted resolution stays behind admin auth and the actions capability", () => {
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  const readOnly: Capabilities = { mode: "static", features: { actions: false } };

  assert.doesNotMatch(renderUnlisted(resolution(), anonymous), /Reopen pattern/);
  assert.doesNotMatch(renderUnlisted(resolution(), admin, readOnly), /Reopen pattern/);
  assert.doesNotMatch(renderUnlisted(resolution(), anonymous, serverCapabilities, "cause"), /Reopen failure/);
  assert.doesNotMatch(renderUnlisted(resolution(), admin, readOnly, "cause"), /Reopen failure/);
});

function failingRun(buildID: string, testName: string): BuildResult {
  return {
    build_id: buildID,
    job_name: "periodic-capz-e2e-main",
    started: "2026-08-18T00:00:00Z",
    finished: "2026-08-18T01:00:00Z",
    passed: false,
    result: "FAILURE",
    duration_seconds: 3600,
    commit: "abc123",
    prow_url: "https://prow.example",
    web_url: "https://gcsweb.example",
    build_log_url: "https://gcsweb.example/build-log.txt",
    tests_total: 1,
    tests_passed: 0,
    tests_failed: 1,
    tests_skipped: 0,
    test_cases: [{
      name: testName,
      status: "failed",
      duration_seconds: 1,
      junit_file: "artifacts/junit_01.xml",
      ai_analysis: {
        generated_at: "2026-08-18T00:00:00Z",
        model: "test",
        root_cause: "cause",
        severity: "high",
        suggested_fix: "fix",
        disposition: "grounded",
        // An analysis with no file links has no verified source path, so the
        // Fix gate refuses it and no route would render.
        file_links: { "a/b.go": "https://github.com/o/r/blob/rev/a/b.go" },
      },
    }],
  };
}

// Both of a cause's actions belong in one bar. Before this they were split by
// the chat accordion, with the route buried mid-body and the resolution below a
// rule, so neither read as the card's set of actions.
test("a cause offers its route and its resolution in one action bar", () => {
  const fixCapable: Capabilities = {
    mode: "server",
    features: { actions: true, analysis_chat: true, junit_chat_fix: true },
    auth: { mode: "oauth" },
  };
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
      ],
    }),
    admin,
    fixCapable,
    undefined,
    [failingRun("100", "[It] Workload cluster creation Creating a highly available cluster")],
  );

  // Both actions render as outlined controls, so the bar reads as a row of
  // actions rather than one button beside a link. The route navigates, so it is
  // an anchor; the resolution posts, so it is a button.
  assert.match(
    html,
    /<button[^>]*class="[^"]*MuiButton-outlined[^"]*"[^>]*>(?:(?!<\/button>)[\s\S])*Resolve failure/,
  );
  // The label carries the humanized test title, so the route names what it
  // opens even once it has shrunk to an ellipsis. It reads as a link, leaving
  // the bordered treatment to the resolution beside it.
  assert.match(
    html,
    /<a[^>]*class="[^"]*MuiButton-text[^"]*"[^>]*aria-label="Highly available cluster in build 100, open representative failure"/,
  );

  // The bar wraps so a resolution error can take its own line, and a wrapping
  // flex line breaks a too-wide item onto a new row rather than shrinking it.
  // The route is sized to its content so it takes only the width it needs, and
  // minWidth: 0 is what still lets it shrink to its ellipsis.
  const routing = source("src/components/CausalGroupFixRouting.tsx");
  assert.match(routing, /flex: "0 1 auto"/);
  assert.match(routing, /minWidth: 0/);
  assert.match(source("src/components/PatternBanner.tsx"), /flexWrap: "wrap"/);

  // They are siblings in one bar: nothing separates them, and the route no
  // longer renders up in the body under the Next step heading.
  const barStart = html.indexOf("open representative failure");
  const resolveAt = html.indexOf("Resolve failure");
  assert.ok(barStart !== -1 && resolveAt > barStart, "the route precedes the resolution in the bar");
  assert.ok(
    html.indexOf("Next step") < barStart,
    "the action bar sits after the body, not inside the Next step section",
  );
});

// Only a resolved cause folds away, so an active one keeps its body on screen
// and gets no toggle at all.
test("an unresolved cause stays open and offers no toggle", () => {
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
      ],
    }),
  );
  assert.match(html, /cni conflict/);

  const banner = source("src/components/PatternBanner.tsx");
  // The toggle and the chevron both render only when collapsible, so an
  // unresolved cause header is plain content rather than a control. An
  // unresolved cause also ignores any override left behind, so a stale entry
  // cannot hide a body that has no toggle left to reveal it.
  assert.match(banner, /const expanded = collapsible \? expandedCauses\[overrideKey\] \?\? false : true/);
  assert.match(banner, /\{collapsible \? \(/);
  assert.match(banner, /\{collapsible && \(\s*\n\s*<ExpandMore/);
});

// The collapsed state depends on resolved.json, which useResolved fetches in an
// effect that renderToStaticMarkup never runs, so the folding rules are pinned
// at the source the same way the other resolved-state rendering is.
test("a resolved cause folds itself away and can be opened again", () => {
  const banner = source("src/components/PatternBanner.tsx");

  // Folding follows resolution state by default, so resolving folds the cause
  // and reopening unfolds it without pinning either.
  assert.match(banner, /const collapsible = Boolean\(causeResolutions\[index\]\)/);
  assert.match(banner, /const expanded = collapsible \? expandedCauses\[overrideKey\] \?\? false : true/);
  // Keyed by signature: the same identity the resolution itself is recorded
  // under, so a refreshed group keeps its fold state. The summary rail's jump
  // link derives the anchor from the same helper.
  assert.match(banner, /return group\.signature \?\? group\.id \?\? String\(index\)/);
  assert.match(banner, /const causeKey = causeKeyFor\(group, index\)/);
  // The heading wraps the toggle rather than containing it, matching
  // AnalysisChat: a heading is not valid phrasing content inside a button and
  // assistive technology flattens that nesting inconsistently. The overlay is
  // what keeps the whole header band clickable.
  assert.match(banner, /<Typography component="h4"[\s\S]*<ButtonBase/);
  assert.match(banner, /aria-expanded=\{expanded\}/);
  assert.match(banner, /aria-controls=\{bodyID\}/);
  assert.match(banner, /"&::after": \{ content: '""', position: "absolute", inset: 0 \}/);
  // unmountOnExit removes the body while collapsed, so the id lives on a
  // wrapper that always renders: otherwise aria-controls would point at nothing
  // in exactly the state the control exists to describe.
  assert.match(banner, /<Box id=\{bodyID\}>\s*\n\s*<Collapse in=\{expanded\}/);
});

// Ownership and the route now render from two different places on the card, so
// this exercises the real composition rather than the halves. Suppressing
// ownership whenever a route existed was a past bug: it made an upstream cause
// indistinguishable from one the project can actually fix.
test("an upstream-owned cause reports ownership in the body and still routes from the bar", () => {
  const fixCapable: Capabilities = {
    mode: "server",
    features: { actions: true, analysis_chat: true, junit_chat_fix: true },
    auth: { mode: "oauth" },
  };
  const html = render(
    causalGroupPattern({
      causal_groups: [{
        builds: ["100"],
        root_cause: "kubelet device manager panics",
        confidence: "high",
        signature: "sig-a",
        cause_location: {
          external: true,
          repository: "kubernetes/kubernetes",
          files: ["pkg/kubelet/cm/devicemanager/manager.go"],
        },
      }],
    }),
    admin,
    fixCapable,
    undefined,
    [failingRun("100", "[It] Workload cluster creation Creating a highly available cluster")],
  );

  assert.match(html, /kubernetes\/kubernetes/);
  assert.match(html, /pkg\/kubelet\/cm\/devicemanager\/manager\.go/);
  assert.match(html, /aria-label="Highly available cluster in build 100, open representative failure"/);
  // Ownership is a diagnosis, not a missing route, so the generic dead end must
  // not appear alongside it.
  assert.doesNotMatch(html, /meets the Fix eligibility requirements/);
});

// A cause with no route at all still explains why, from the body, while the bar
// falls back to the resolution control on its own.
test("a cause with no route keeps its dead-end explanation and still resolves", () => {
  const fixCapable: Capabilities = {
    mode: "server",
    features: { actions: true, analysis_chat: true, junit_chat_fix: true },
    auth: { mode: "oauth" },
  };
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
      ],
    }),
    admin,
    fixCapable,
    undefined,
    // A run whose failure carries no file links fails the Fix gate, so the
    // cause is in the window but has no eligible representative.
    [(() => {
      const run = failingRun("100", "[It] Workload cluster creation Creating a highly available cluster");
      delete run.test_cases[0].ai_analysis!.file_links;
      return run;
    })()],
  );

  assert.match(html, /meets the Fix eligibility requirements/);
  assert.doesNotMatch(html, /aria-label="[^"]*, open representative failure/);
  assert.match(html, /Resolve failure/);
});

// The route moved to the action bar, so routing is no longer content for the
// Next step section on its own. A cause with a route but nothing to say would
// otherwise render a bare heading above the bar.
test("a cause with a route but no remediation and no chat renders no Next step heading", () => {
  const fixCapable: Capabilities = {
    mode: "server",
    features: { actions: true, analysis_chat: true, junit_chat_fix: true },
    auth: { mode: "oauth" },
  };
  const html = render(
    causalGroupPattern({
      causal_groups: [
        { builds: ["100"], root_cause: "cni conflict", confidence: "high", signature: "sig-a" },
      ],
    }),
    admin,
    fixCapable,
    undefined,
    [failingRun("100", "[It] Workload cluster creation Creating a highly available cluster")],
  );

  // The route still renders, from the bar.
  assert.match(html, /aria-label="[^"]*, open representative failure/);
  assert.doesNotMatch(html, /Next step/);
});

// Resolving, reopening, then resolving again must fold the card away again.
// Keying the override on the resolution event is what discards the stale one.
test("an expansion override belongs to one resolution event", () => {
  const banner = source("src/components/PatternBanner.tsx");

  assert.match(
    banner,
    /const overrideKey = `\$\{causeKey\}:\$\{causeResolutions\[index\]\?\.resolved_at \?\? ""\}`/,
  );
  assert.match(banner, /expandedCauses\[overrideKey\] \?\? false/);
  assert.match(banner, /\[overrideKey\]: !expanded/);
});
