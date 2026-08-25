import assert from "node:assert/strict";
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
import type { FlakinessReport, PatternAnalysis, PatternRefreshStatus, ResolvedEntry, ResolvedState } from "../src/types/dashboard.js";
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
          createElement(PatternBanner, { pattern, jobID: pattern.job_id, refreshStatus }),
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
});

test("reopening an unlisted resolution stays behind admin auth and the actions capability", () => {
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  const readOnly: Capabilities = { mode: "static", features: { actions: false } };

  assert.doesNotMatch(renderUnlisted(resolution(), anonymous), /Reopen pattern/);
  assert.doesNotMatch(renderUnlisted(resolution(), admin, readOnly), /Reopen pattern/);
  assert.doesNotMatch(renderUnlisted(resolution(), anonymous, serverCapabilities, "cause"), /Reopen failure/);
  assert.doesNotMatch(renderUnlisted(resolution(), admin, readOnly, "cause"), /Reopen failure/);
});
