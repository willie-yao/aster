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

const fixCapable: Capabilities = {
  mode: "server",
  features: { actions: true, analysis_chat: true, junit_chat_fix: true },
};

function failedTest(name: string): TestCase {
  return {
    name,
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
}

function run(buildID: string, testName: string): BuildResult {
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
    test_cases: [failedTest(testName)],
    tests_total: 1,
    tests_passed: 0,
    tests_failed: 1,
    tests_skipped: 0,
  };
}

// Causes 1 and 2 carry DIFFERENT canonical test names that humanize to the SAME
// display title. Counting canonical names would see two unique labels and hide
// both builds, leaving two identical buttons.
const collidingA = "[It] Workload cluster creation Creating a highly available cluster";
const collidingB = "[It] Running the Cluster API E2E tests Highly available cluster";
const distinct = "[It] Conformance Tests conformance tests should pass";

const runs = [run("100", collidingA), run("250", collidingB), run("300", distinct)];

const pattern: PatternAnalysis = {
  id: "pattern-1",
  content_hash: "hash",
  subject: "periodic-capz-e2e-main",
  job_id: "periodic-capz-e2e-main",
  generated_at: "2026-08-18T00:00:00Z",
  builds_analyzed: 3,
  recurrence_classification: "mixed_causes",
  causal_groups: [
    { id: "g1", content_hash: "h1", builds: ["100"], root_cause: "first", confidence: "high" },
    { id: "g2", content_hash: "h2", builds: ["250"], root_cause: "second", confidence: "medium" },
    { id: "g3", content_hash: "h3", builds: ["300"], root_cause: "third", confidence: "low" },
  ],
  systemic: true,
  confidence: "medium",
  summary: "Three causes.",
};

function render(): string {
  const tree: ReactNode = createElement(
    ThemeProvider,
    { theme: defaultTheme },
    createElement(
      MemoryRouter,
      null,
      createElement(
        CapabilitiesContext.Provider,
        { value: fixCapable },
        createElement(PatternBanner, { pattern, jobID: pattern.job_id, runs }),
      ),
    ),
  );
  return renderToStaticMarkup(tree);
}

function decode(value: string): string {
  return value
    .replaceAll("&lt;", "<")
    .replaceAll("&gt;", ">")
    .replaceAll("&quot;", '"')
    .replaceAll("&#x27;", "'")
    .replaceAll("&amp;", "&");
}

interface FixAction {
  visible: string;
  accessible: string;
}

function fixActions(html: string): FixAction[] {
  const actions: FixAction[] = [];
  for (const match of html.matchAll(/<a\b[^>]*aria-label="([^"]*)"[^>]*>([\s\S]*?)<\/a>/gu)) {
    const accessible = decode(match[1]);
    if (!accessible.startsWith("Fix:")) continue;
    // Emotion inlines a <style> block inside the first rendered anchor, so its
    // CSS text has to go before tags are stripped or it lands in the label.
    const body = match[2].replace(/<style\b[\s\S]*?<\/style>/gu, "");
    actions.push({ visible: decode(body.replace(/<[^>]*>/gu, "")).trim(), accessible });
  }
  return actions;
}

test("every Fix action renders a visible label that is a literal prefix of its accessible name", () => {
  const actions = fixActions(render());

  assert.equal(actions.length, 3);
  for (const action of actions) {
    // WCAG 2.5.3. Compared byte for byte on the real markup: an NBSP separator
    // in the markup against an ASCII space in the accessible name silently
    // breaks this while still looking correct on screen.
    assert.ok(
      action.accessible.startsWith(action.visible),
      `accessible name ${JSON.stringify(action.accessible)} must start with visible label ${JSON.stringify(action.visible)}`,
    );
  }
});

test("Fix actions stay distinguishable when two causes humanize to one title", () => {
  const actions = fixActions(render());
  const visible = actions.map((action) => action.visible);

  assert.equal(new Set(visible).size, 3, `visible labels must be unique, got ${JSON.stringify(visible)}`);
  assert.equal(new Set(actions.map((action) => action.accessible)).size, 3);

  // The two colliding causes each carry their build; the distinct one does not
  // pay for 19 digits it does not need.
  assert.deepEqual(
    visible.map((label) => label.includes(" in build ")),
    [true, true, false],
  );
  assert.ok(visible[0].startsWith("Fix: Highly available cluster in build 100"));
  assert.ok(visible[1].startsWith("Fix: Highly available cluster in build 250"));  assert.equal(visible[2], "Fix: Conformance tests should pass");
});

test("the Fix action names the test the way the rest of the page does", () => {
  const actions = fixActions(render());

  // The raw JUnit name and its suite prefix never reach the label.
  for (const action of actions) {
    assert.doesNotMatch(action.visible, /\[It\]/);
    assert.doesNotMatch(action.visible, /Workload cluster creation/);
    assert.doesNotMatch(action.visible, /Running the Cluster API E2E tests/);
  }
});
