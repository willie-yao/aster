import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import { unlistedDismissals } from "../src/lib/dashboardOverview.js";
import type { FlakinessReport, PatternAnalysis, PatternRefreshStatus, ResolvedEntry } from "../src/types/dashboard.js";
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
const { UnlistedDismissalRow } = (await vite.ssrLoadModule("/src/components/NeedsAttention.tsx")) as {
  UnlistedDismissalRow: (props: {
    failureID: string;
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
  assert.match(html, /Dismiss pattern/);
  // Pattern-level drafting stays blocked for causal-group results, and the
  // pattern-level eligibility notice stays out of the way.
  assert.doesNotMatch(html, /Draft issue/);
  assert.doesNotMatch(html, /Draft fix PR/);
  assert.doesNotMatch(html, /Preview generation failed/);
});

test("dismissal follows the same gates the server enforces", () => {
  assert.doesNotMatch(render(causalGroupPattern({ systemic: false })), /Dismiss pattern/);
  assert.doesNotMatch(render(causalGroupPattern({ id: undefined })), /Dismiss pattern/);
  assert.doesNotMatch(
    render(causalGroupPattern({ lifecycle: { state: "verified_fixed", reason: "verified fixed" } })),
    /Dismiss pattern/,
  );
  // The server derives the recurrence watermark from shared_builds.
  assert.doesNotMatch(render(causalGroupPattern({ shared_builds: [] })), /Dismiss pattern/);
  // findPattern rejects a stale refresh or evidence that left the job window.
  assert.match(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "current", evidence_available: true }),
    /Dismiss pattern/,
  );
  assert.doesNotMatch(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "current", evidence_available: false }),
    /Dismiss pattern/,
  );
  // A retained correlation still describes the published pattern, so dismissal
  // stays available while its evidence is readable.
  assert.match(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "retained", evidence_available: true }),
    /Dismiss pattern/,
  );
  assert.doesNotMatch(
    render(causalGroupPattern(), admin, serverCapabilities, { state: "retained", evidence_available: false }),
    /Dismiss pattern/,
  );
});

test("dismissal stays behind admin auth and the actions capability", () => {
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  assert.doesNotMatch(render(causalGroupPattern(), anonymous), /Dismiss pattern/);

  const readOnly: Capabilities = { mode: "static", features: { actions: false } };
  assert.doesNotMatch(render(causalGroupPattern(), admin, readOnly), /Dismiss pattern/);
});

test("the pattern-level eligibility notice stays with the legacy contract", () => {
  // A legacy pattern with no remediation targets gets a deterministic blocked
  // hint, so the drafting notice renders and dismissal is still offered.
  const legacy = causalGroupPattern({ recurrence_classification: undefined, causal_groups: undefined });
  const legacyHtml = render(legacy);
  assert.match(legacyHtml, /Preview generation failed/);
  assert.match(legacyHtml, /Dismiss pattern/);

  // The same notice is suppressed on a causal-group result, where pattern-level
  // drafting does not apply and per-cause remediation is shown instead.
  assert.doesNotMatch(render(causalGroupPattern()), /Preview generation failed/);
});

// A dismissal whose pattern has left the active recurring set is retained by
// the fetcher but disappears from the overview, so the dismissed-patterns
// disclosure lists it with the restore path.

function reportWith(ids: string[]): FlakinessReport {
  return {
    generated_at: "2026-08-18T00:00:00Z",
    most_flaky: [],
    persistent_failures: [],
    recently_broken: [],
    build_failures: [],
    recurring_patterns: ids.map((id) => causalGroupPattern({ id })),
  };
}

function dismissal(overrides: Partial<ResolvedEntry> = {}): ResolvedEntry {
  return {
    resolved_at: "2026-08-17T00:00:00Z",
    resolved_by: "willie-yao",
    watermark: "250",
    subject: "periodic-capz-e2e-main",
    ...overrides,
  };
}

// recurring_patterns comes from patterns.CollectRecurring, which publishes only
// systemic, lifecycle-active patterns and does not truncate. So an id missing
// from it is either a pattern that aged out or one whose lifecycle moved on,
// and the overview, which reads nothing else, cannot tell them apart.
test("only dismissals absent from the active recurring set are listed as unlisted", () => {
  const report = reportWith(["pattern-1"]);
  const state = { resolved: { "pattern-1": dismissal(), "pattern-gone": dismissal() } };

  assert.deepEqual(
    unlistedDismissals(report, state, true).map(([id]) => id),
    ["pattern-gone"],
  );
});

test("unlisted dismissals are withheld where they could not be restored", () => {
  const state = { resolved: { "pattern-gone": dismissal() } };

  // An unread report cannot distinguish an unlisted pattern from an unread one.
  assert.deepEqual(unlistedDismissals(null, state, true), []);
  // Restoring is the only thing a viewer can do with one, so a read-only or
  // signed-out viewer is shown nothing rather than a row that cannot act.
  assert.deepEqual(unlistedDismissals(reportWith([]), state, false), []);
});

function renderUnlisted(
  entry: ResolvedEntry,
  auth: AuthState = admin,
  capabilities = serverCapabilities,
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
          createElement(UnlistedDismissalRow, {
            failureID: "pattern-gone",
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

test("an unlisted dismissal offers Restore and explains why it has no analysis", () => {
  const html = renderUnlisted(dismissal({ note: "fixed upstream in kubernetes/kubernetes#1" }));

  assert.match(html, /Restore pattern/);
  assert.match(html, /Dismissed by willie-yao/);
  assert.match(html, /no longer among the active recurring failures/);
  assert.match(html, /fixed upstream/);
  // The pattern left the active set, so there is nothing to dismiss again and
  // no pattern-level analysis to draft from.
  assert.doesNotMatch(html, /Dismiss pattern/);
  assert.doesNotMatch(html, /Draft issue/);
  // resolved.json records no job and the overview cannot confirm the pattern is
  // still published anywhere, so the row has no destination to offer. That also
  // keeps the Restore button out of a surrounding link.
  assert.doesNotMatch(html, /<a /);
});

test("restoring an unlisted dismissal stays behind admin auth and the actions capability", () => {
  const anonymous: AuthState = { ...admin, status: "anonymous", login: null };
  assert.doesNotMatch(renderUnlisted(dismissal(), anonymous), /Restore pattern/);

  const readOnly: Capabilities = { mode: "static", features: { actions: false } };
  assert.doesNotMatch(renderUnlisted(dismissal(), admin, readOnly), /Restore pattern/);
});
