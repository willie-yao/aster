import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { createServer } from "vite";
import type { BuildResult, PatternAnalysis, TestCase } from "../src/types/dashboard.js";
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
    runs?: BuildResult[];
  }) => ReturnType<typeof createElement>;
};
const { CapabilitiesContext } = (await vite.ssrLoadModule("/src/hooks/useCapabilities.ts")) as {
  CapabilitiesContext: React.Context<Capabilities>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
await vite.close();

// A static Pages deploy: no chat, no fix routing, no remediation investigation.
const published: Capabilities = { mode: "static", features: { actions: false } };
const fixCapable: Capabilities = {
  mode: "server",
  features: { actions: true, analysis_chat: true, junit_chat_fix: true },
};

const failedTest: TestCase = {
  name: "[It] Conformance Tests conformance tests should pass",
  status: "failed",
  duration_seconds: 1,
  junit_file: "artifacts/junit_01.xml",
  ai_analysis: {
    generated_at: "2026-08-18T00:00:00Z",
    model: "test",
    root_cause: "cause",
    severity: "high",
    suggested_fix: "fix",
    file_links: { "a/b.go": "https://github.com/o/r/blob/rev/a/b.go" },
  },
};

const run: BuildResult = {
  build_id: "300",
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
  test_cases: [failedTest],
  tests_total: 1,
  tests_passed: 0,
  tests_failed: 1,
  tests_skipped: 0,
};

function pattern(options: { classified: boolean; reportedFix: boolean }): PatternAnalysis {
  return {
    id: "pattern-1",
    content_hash: "hash",
    subject: "periodic-capz-e2e-main",
    job_id: "periodic-capz-e2e-main",
    generated_at: "2026-08-18T00:00:00Z",
    builds_analyzed: 3,
    recurrence_classification: options.classified ? "shared_cause" : undefined,
    causal_groups: [
      {
        id: "g1",
        content_hash: "h1",
        builds: ["300"],
        root_cause: "the node image is out of date",
        confidence: "high",
        remediation: options.reportedFix
          ? { suggested_fix: "Bump the node image to the current release.", build_id: "300" }
          : undefined,
      },
    ],
    systemic: true,
    confidence: "medium",
    summary: "One cause.",
  };
}

function render(analysis: PatternAnalysis, capabilities: Capabilities): string {
  const tree: ReactNode = createElement(
    ThemeProvider,
    { theme: defaultTheme },
    createElement(
      MemoryRouter,
      null,
      createElement(
        CapabilitiesContext.Provider,
        { value: capabilities },
        createElement(PatternBanner, { pattern: analysis, jobID: analysis.job_id, runs: [run] }),
      ),
    ),
  );
  return renderToStaticMarkup(tree);
}

test("a cause gathers everything it offers under one Next step section", () => {
  const html = render(pattern({ classified: true, reportedFix: true }), fixCapable);

  assert.equal(html.match(/Next step/g)?.length, 1);
  assert.match(html, /Suggested remediation from build 300/);
  assert.match(html, /Bump the node image to the current release\./);
  assert.match(html, /Open representative failure: Conformance tests should pass/);
  assert.match(html, /Implementation target/);

  // The old framing stamped the same sentence the test detail page presents
  // plainly as untrustworthy, and named a verdict the control cannot give.
  assert.doesNotMatch(html, /Unverified suggested fix/);
  assert.doesNotMatch(html, /Verified fix investigation/);
});

test("a published-only deploy still gets the cause's reported remediation", () => {
  const html = render(pattern({ classified: false, reportedFix: true }), published);

  assert.match(html, /Next step/);
  assert.match(html, /Suggested remediation from build 300/);
  assert.match(html, /Bump the node image to the current release\./);
  // Neither capability is present, so neither control may be offered.
  assert.doesNotMatch(html, /Open representative failure/);
  assert.doesNotMatch(html, /Implementation target/);
});

test("a cause with nothing to offer renders no Next step section at all", () => {
  const html = render(pattern({ classified: false, reportedFix: false }), published);

  assert.match(html, /the node image is out of date/);
  assert.doesNotMatch(html, /Next step/);
});
