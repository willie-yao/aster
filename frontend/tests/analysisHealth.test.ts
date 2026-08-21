import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createServer } from "vite";
import * as ts from "typescript";
import {
  analysisHealthCounts,
  analysisHealthVerdict,
  needsAttention,
  rankAnalysisHealth,
} from "../src/lib/analysisHealth.js";
import {
  analysisTraceActiveFilterCount,
  analysisTraceEventDetails,
  analysisTraceResponseIDs,
  formatTraceDuration,
  traceStatusLabel,
  traceTone,
} from "../src/lib/analysisTraces.js";
import type { AnalysisTrace, AnalysisTraceEvent } from "../src/types/traces.js";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const { AnalysisTraceLedger, CopyIdentifierAction, TraceEventRow } = (await vite.ssrLoadModule(
  "/src/components/AnalysisTraceLedger.tsx",
)) as {
  AnalysisTraceLedger: (props: {
    title: string;
    description?: string;
    metadata?: string;
    items: Array<{
      trace: AnalysisTrace;
      verdict: ReturnType<typeof analysisHealthVerdict>;
      displayTitle: string;
      displayJob: string;
      testHref: string;
      responseIDs: string[];
    }>;
  }) => ReturnType<typeof createElement>;
  CopyIdentifierAction: (props: {
    label: string;
    value: string;
    copied: boolean;
    onCopy: () => void;
  }) => ReturnType<typeof createElement>;
  TraceEventRow: (props: { event: AnalysisTraceEvent }) => ReturnType<typeof createElement>;
};
const { AnalysisTraceFilters } = (await vite.ssrLoadModule(
  "/src/components/AnalysisTraceFilters.tsx",
)) as {
  AnalysisTraceFilters: (props: {
    searchParams: URLSearchParams;
    onApply: (params: URLSearchParams) => void;
    onClear: () => void;
  }) => ReturnType<typeof createElement>;
};
const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
};
await vite.close();

const ledgerSource = readFileSync(
  resolve(process.cwd(), "src/components/AnalysisTraceLedger.tsx"),
  "utf8",
);
const ledgerSourceFile = ts.createSourceFile(
  "AnalysisTraceLedger.tsx",
  ledgerSource,
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TSX,
);

function interactiveKind(node: ts.JsxOpeningLikeElement): "button" | "link" | null {
  const component = node.attributes.properties.find(
    (property): property is ts.JsxAttribute =>
      ts.isJsxAttribute(property) && property.name.getText(ledgerSourceFile) === "component",
  );
  const renderedComponent = component?.initializer?.getText(ledgerSourceFile) ?? "";
  if (renderedComponent === '"a"' || renderedComponent === "{RouterLink}") return "link";
  if (renderedComponent === '"button"') return "button";

  const name = node.tagName.getText(ledgerSourceFile);
  if (name === "Button" || name === "ButtonBase" || name === "button") return "button";
  if (name === "Link" || name === "RouterLink" || name === "a") return "link";
  return null;
}

function collectInteractiveNesting(): string[] {
  const violations: string[] = [];

  function visit(node: ts.Node, ancestors: Array<"button" | "link">) {
    if (ts.isJsxElement(node)) {
      const current = interactiveKind(node.openingElement);
      if (current && ancestors.length > 0) {
        violations.push(`${ancestors.at(-1)} contains ${current}`);
      }
      const next = current ? [...ancestors, current] : ancestors;
      node.children.forEach((child) => visit(child, next));
      return;
    }
    if (ts.isJsxSelfClosingElement(node)) {
      const current = interactiveKind(node);
      if (current && ancestors.length > 0) {
        violations.push(`${ancestors.at(-1)} contains ${current}`);
      }
      return;
    }
    ts.forEachChild(node, (child) => visit(child, ancestors));
  }

  visit(ledgerSourceFile, []);
  return violations;
}

function render(element: ReturnType<typeof createElement>): string {
  return renderToStaticMarkup(
    createElement(
      ThemeProvider,
      { theme: defaultTheme },
      createElement(MemoryRouter, null, element),
    ),
  );
}

function trace(overrides: Partial<AnalysisTrace> = {}): AnalysisTrace {
  return {
    job_id: "periodic-cluster-api-provider-azure-e2e-main",
    build_id: "20134789654",
    test_name: "[It] Workload cluster creation Creating a highly-available cluster",
    api_mode: "responses",
    started_at: "2026-08-10T18:31:04Z",
    elapsed_ms: 84231,
    outcome: "success",
    events: [
      {
        sequence: 1,
        elapsed_ms: 412,
        kind: "model_request",
        outcome: "completed",
        response_id: "resp-one",
      },
      {
        sequence: 2,
        elapsed_ms: 840,
        kind: "model_request",
        outcome: "completed",
        response_id: "resp-two",
      },
      {
        sequence: 3,
        elapsed_ms: 920,
        kind: "model_request",
        outcome: "completed",
        response_id: "resp-one",
      },
    ],
    ...overrides,
  };
}

function ledgerItem(overrides: Partial<AnalysisTrace> = {}) {
  const value = trace(overrides);
  return {
    trace: value,
    verdict: analysisHealthVerdict(value),
    displayTitle: "Highly-available cluster",
    displayJob: "e2e-main",
    testHref: "/job/job-main/test/test-name?run=20134789654",
    responseIDs: analysisTraceResponseIDs(value),
  };
}

test("trace helpers preserve query, status, duration, and response semantics", () => {
  assert.equal(
    analysisTraceActiveFilterCount(
      new URLSearchParams("job_id=job&build_id=&test_name=test&response_id=resp"),
    ),
    3,
  );
  assert.equal(formatTraceDuration(412), "412 ms");
  assert.equal(formatTraceDuration(3840), "3.84 s");
  assert.equal(formatTraceDuration(121884), "121.9 s");
  assert.equal(traceStatusLabel("http_error"), "HTTP error");
  assert.equal(traceTone("completed"), "success");
  assert.equal(traceTone("retry"), "warning");
  assert.equal(traceTone("retry_exhausted"), "warning");
  assert.equal(traceTone("quality_failed"), "error");
  assert.deepEqual(analysisTraceResponseIDs(trace()), ["resp-one", "resp-two"]);
});

test("event details retain every existing public metadata field in order", () => {
  const event: AnalysisTraceEvent = {
    sequence: 4,
    elapsed_ms: 4711,
    kind: "model_request",
    outcome: "retry",
    response_id: "resp-123",
    tool: "read_artifact",
    status: "ok",
    finish_reason: "tool_calls",
    duration_ms: 6421,
    attempts: 2,
    http_status: 200,
    input_tokens: 26780,
    output_tokens: 1440,
    message_count: 8,
    tool_call_count: 2,
    bytes: 48219,
    elided: 12,
    retry: 1,
    issue_count: 2,
    critique_rules: ["missing_citation"],
    semantic_findings: ["unsupported_cause"],
    cache_rejection_reason: "below_floor",
    validation_code: "schema_mismatch",
    error_code: "quality_floor_exhausted",
  };

  assert.deepEqual(analysisTraceEventDetails(event), [
    "response resp-123",
    "read_artifact",
    "status ok",
    "finish tool_calls",
    "request 6.42 s",
    "2 attempts",
    "HTTP 200",
    "26780 in / 1440 out",
    "8 messages",
    "2 tool calls",
    "48,219 bytes",
    "12 elided",
    "retry 1",
    "2 issues",
    "rules missing_citation",
    "findings unsupported_cause",
    "not cached: below_floor",
    "schema_mismatch",
    "quality_floor_exhausted",
  ]);
});

test("health classification separates failures, degradations, retries, and clean runs", () => {
  const clean = analysisHealthVerdict(trace());
  assert.equal(clean.severity, "healthy");
  assert.deepEqual(clean.reasons, []);
  assert.equal(clean.modelRequests, 3);
  assert.equal(clean.toolCalls, 0);

  const failed = analysisHealthVerdict(
    trace({ outcome: "unavailable", error_code: "provider_status" }),
  );
  assert.equal(failed.severity, "failed");
  assert.deepEqual(failed.reasons, ["Analysis unavailable: provider status"]);

  const degraded = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "floor_nudge", outcome: "retry_exhausted" },
        { sequence: 2, elapsed_ms: 20, kind: "critique_retry_denied", outcome: "retry_budget" },
      ],
    }),
  );
  assert.equal(degraded.severity, "degraded");
  assert.deepEqual(degraded.reasons, [
    "Exhausted quality-floor retries",
    "Critique retry denied: retry budget",
  ]);

  const retried = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "critique", outcome: "objected", issue_count: 2 },
        { sequence: 2, elapsed_ms: 20, kind: "critique_retry", outcome: "completed" },
        { sequence: 3, elapsed_ms: 30, kind: "semantic_judge", outcome: "revised" },
        { sequence: 4, elapsed_ms: 40, kind: "critique", outcome: "published_passed" },
      ],
    }),
  );
  assert.equal(retried.severity, "retried");
  assert.deepEqual(retried.reasons, [
    "Critique objected: 2 issues",
    "Critique retry: completed",
    "Semantic judge revised the draft",
  ]);

  const rejected = analysisHealthVerdict(
    trace({
      events: [{ sequence: 1, elapsed_ms: 10, kind: "critique", outcome: "published_rejected", issue_count: 3 }],
    }),
  );
  assert.equal(rejected.severity, "degraded");
  assert.deepEqual(rejected.reasons, ["Critique rejected the published analysis: 3 issues"]);

  assert.equal(
    analysisHealthVerdict(
      trace({ events: [{ sequence: 1, elapsed_ms: 10, kind: "critique", outcome: "published_passed" }] }),
    ).severity,
    "healthy",
  );

  assert.equal(needsAttention("failed"), true);
  assert.equal(needsAttention("degraded"), true);
  assert.equal(needsAttention("retried"), false);
  assert.equal(needsAttention("healthy"), false);
});

test("model and tool events fold provider errors and evidence budgets into health", () => {
  const verdict = analysisHealthVerdict(
    trace({
      truncated: true,
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "model_request", http_status: 503 },
        { sequence: 2, elapsed_ms: 20, kind: "tool_call", tool: "read", outcome: "success", bytes: 4096 },
        { sequence: 3, elapsed_ms: 25, kind: "tool_call", tool: "read", outcome: "error" },
        { sequence: 4, elapsed_ms: 30, kind: "tool_call", tool: "read", outcome: "gcs_budget_exhausted" },
      ],
    }),
  );

  assert.equal(verdict.severity, "degraded");
  assert.equal(verdict.modelRequests, 1);
  assert.equal(verdict.toolCalls, 3);
  assert.equal(verdict.toolErrors, 1);
  assert.equal(verdict.evidenceBytes, 4096);
  assert.deepEqual(verdict.reasons, [
    "Trace recording truncated",
    "Model request returned HTTP 503",
    "Evidence gathering stopped: gcs budget exhausted",
  ]);

  // A tool error alone is normal exploration and must not degrade the analysis.
  const explored = analysisHealthVerdict(
    trace({ events: [{ sequence: 1, elapsed_ms: 10, kind: "tool_call", tool: "read", outcome: "error" }] }),
  );
  assert.equal(explored.severity, "healthy");
  assert.equal(explored.toolErrors, 1);
});

test("ranking orders by severity then recency and counts each severity", () => {
  const traces = [
    trace({ build_id: "healthy-old", recorded_at: "2026-08-01T00:00:00Z" }),
    trace({ build_id: "failed", outcome: "error", recorded_at: "2026-08-02T00:00:00Z" }),
    trace({ build_id: "healthy-new", recorded_at: "2026-08-09T00:00:00Z" }),
    trace({
      build_id: "degraded",
      recorded_at: "2026-08-03T00:00:00Z",
      events: [{ sequence: 1, elapsed_ms: 5, kind: "publication", outcome: "unavailable" }],
    }),
  ];

  const ranked = rankAnalysisHealth(traces, (value) => value);
  assert.deepEqual(
    ranked.map((entry) => entry.item.build_id),
    ["failed", "degraded", "healthy-new", "healthy-old"],
  );
  assert.deepEqual(analysisHealthCounts(ranked), {
    failed: 1,
    degraded: 1,
    retried: 0,
    healthy: 2,
  });
});

test("context headroom and structured fallbacks separate recovery from real loss", () => {
  const headroom = analysisHealthVerdict(
    trace({
      events: [{ sequence: 1, elapsed_ms: 10, kind: "context_headroom", outcome: "best_draft" }],
    }),
  );
  assert.equal(headroom.severity, "degraded");
  assert.deepEqual(headroom.reasons, ["Fell back to the best draft: no context headroom"]);

  const overBudget = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 5, kind: "context_compaction", outcome: "loop", elided: 2 },
        { sequence: 2, elapsed_ms: 10, kind: "context_headroom", outcome: "over_budget" },
      ],
    }),
  );
  assert.equal(overBudget.severity, "degraded");
  // Compaction must not claim it fit when the very next event says it did not.
  assert.deepEqual(overBudget.reasons, [
    "Still over the context budget after compaction",
    "Conversation compacted",
  ]);

  const judgeFailed = analysisHealthVerdict(
    trace({ events: [{ sequence: 1, elapsed_ms: 10, kind: "semantic_judge", outcome: "error" }] }),
  );
  assert.equal(judgeFailed.severity, "degraded");
  assert.deepEqual(judgeFailed.reasons, ["Semantic judge failed to run"]);

  const failedRepair = analysisHealthVerdict(
    trace({ events: [{ sequence: 1, elapsed_ms: 10, kind: "critique_retry", outcome: "unparseable" }] }),
  );
  assert.equal(failedRepair.severity, "degraded");
  assert.deepEqual(failedRepair.reasons, ["Critique repair failed: unparseable"]);

  const recoveredStructured = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "structured_completion", structured_outcome: "invalid_json" },
        { sequence: 2, elapsed_ms: 20, kind: "structured_completion", structured_outcome: "accepted" },
      ],
    }),
  );
  assert.equal(recoveredStructured.severity, "retried");
  assert.deepEqual(recoveredStructured.reasons, ["Recovered from structured output invalid json"]);

  const lostStructured = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "structured_completion", structured_outcome: "provider_error" },
        { sequence: 2, elapsed_ms: 20, kind: "structured_completion", structured_outcome: "no_candidate" },
      ],
    }),
  );
  assert.equal(lostStructured.severity, "degraded");
  assert.deepEqual(lostStructured.reasons, ["Never accepted structured output provider error, no candidate"]);

  // An acceptance only clears the failures that preceded it, so a later cycle
  // that never succeeds still degrades the analysis.
  const acceptedThenLost = analysisHealthVerdict(
    trace({
      events: [
        { sequence: 1, elapsed_ms: 10, kind: "structured_completion", structured_outcome: "accepted" },
        { sequence: 2, elapsed_ms: 20, kind: "structured_completion", structured_outcome: "provider_error" },
        { sequence: 3, elapsed_ms: 30, kind: "structured_completion", structured_outcome: "no_candidate" },
      ],
    }),
  );
  assert.equal(acceptedThenLost.severity, "degraded");
  assert.deepEqual(acceptedThenLost.reasons, [
    "Never accepted structured output provider error, no candidate",
  ]);
});

test("trace filters disclose active URL state and retain all supported fields", () => {
  const html = render(createElement(AnalysisTraceFilters, {
    searchParams: new URLSearchParams("job_id=job-main&build_id=123"),
    onApply: () => undefined,
    onClear: () => undefined,
  }));

  assert.match(html, /aria-label="Trace filters"/);
  assert.match(html, /aria-expanded="true"/);
  assert.match(html, />2 active</);
  assert.match(html, /name="job_id"/);
  assert.match(html, /name="build_id"/);
  assert.match(html, /name="test_name"/);
  assert.match(html, /name="response_id"/);
  assert.match(html, />Clear all</);
  assert.match(html, /Download JSON uses the current URL filters/);
});

test("health ledger leads with severity and why, and keeps route and copy actions separate", () => {
  const html = render(
    createElement(AnalysisTraceLedger, {
      title: "Failed",
      description: "No usable analysis was published for these failures.",
      items: [ledgerItem({ outcome: "error", error_code: "deadline_exceeded" })],
    }),
  );

  assert.match(html, /<h2[^>]*>Failed<\/h2>/);
  assert.match(html, />1 analysis</);
  assert.match(html, /No usable analysis was published for these failures\./);
  assert.match(html, /aria-expanded="false"/);
  assert.match(html, /aria-controls="analysis-trace-/);
  assert.match(html, /aria-label="Expand Failed analysis for Highly-available cluster\./);
  assert.match(html, /Job e2e-main\. Build 20134789654\. Analysis error: deadline exceeded\. 84\.2 s\. 3 events\./);
  assert.match(html, /Analysis error: deadline exceeded/);
  assert.match(html, /title="\[It\] Workload cluster creation Creating a highly-available cluster"/);
  assert.match(html, /title="periodic-cluster-api-provider-azure-e2e-main"/);
  assert.deepEqual(collectInteractiveNesting(), []);
});

test("expanded health rows expose the test route and copyable identifiers", () => {
  const html = render(
    createElement(AnalysisTraceLedger, { title: "Healthy", items: [ledgerItem()] }),
  );

  assert.match(html, /Completed without intervention/);
  assert.match(html, /aria-label="Expand Healthy analysis for Highly-available cluster\./);
  assert.doesNotMatch(html, /href="\/job\/job-main/);
});

test("mobile event rows keep sequence elapsed kind outcome and details visible", () => {
  const event: AnalysisTraceEvent = {
    sequence: 2,
    elapsed_ms: 412,
    kind: "model_request",
    outcome: "completed",
    response_id: "resp-one",
    duration_ms: 3860,
  };
  const html = render(createElement(TraceEventRow, { event }));

  assert.match(html, />02</);
  assert.match(html, />\+412 ms</);
  assert.match(html, />model_request</);
  assert.match(html, />Completed</);
  assert.match(html, /response resp-one · request 3\.86 s/);
});

test("copy completion is exposed through the button name and a polite status", () => {
  const html = render(createElement(CopyIdentifierAction, {
    label: "Build",
    value: "20134789654",
    copied: true,
    onCopy: () => undefined,
  }));

  assert.match(html, /aria-label="Build 20134789654 copied"/);
  assert.match(html, /role="status"/);
  assert.match(html, /aria-live="polite"/);
  assert.match(html, />Build copied</);
});

test("blank identifiers do not render empty copy actions", () => {
  const html = render(createElement(CopyIdentifierAction, {
    label: "Build",
    value: "",
    copied: false,
    onCopy: () => undefined,
  }));

  assert.doesNotMatch(html, /<button/);
  assert.doesNotMatch(html, />Build</);
});

test("trace detail empty states stay contained and explicit", () => {
  assert.match(ledgerSource, /width: "1px"[\s\S]*height: "1px"[\s\S]*clipPath: "inset\(50%\)"/);
  assert.match(ledgerSource, /testHref && trace\.build_id\.trim\(\) && trace\.test_name\.trim\(\)/);
  assert.match(ledgerSource, /No trace events were recorded\./);
});

test("Analysis health page preserves private gates, problem-first grouping, and downloads", () => {
  const source = readFileSync(resolve(process.cwd(), "src/pages/AnalysisHealthPage.tsx"), "utf8");
  const filters = readFileSync(resolve(process.cwd(), "src/components/AnalysisTraceFilters.tsx"), "utf8");
  const ledger = readFileSync(resolve(process.cwd(), "src/components/AnalysisTraceLedger.tsx"), "utf8");

  assert.match(source, />\s*Analysis Health\s*</);
  assert.doesNotMatch(source, /gridArea: "description"/);
  assert.match(source, /About trace privacy/);
  assert.match(source, /aria-expanded=\{open\}/);
  assert.match(source, /aria-controls=\{contentID\}/);
  assert.match(source, /Prompts, tool arguments, tool results, credentials, diagnostic content, and billing records are never shown/);
  assert.match(source, /if \(!features\.analysis_health\)/);
  assert.match(source, /if \(auth\.status === "loading"\)/);
  assert.match(source, /if \(auth\.status === "anonymous"\)/);
  assert.match(source, /api\/analysis-health\$\{query \? `\?\$\{query\}` : ""\}/);
  assert.match(source, /api\/analysis-health\/download\$\{query \? `\?\$\{query\}` : ""\}/);
  assert.match(source, /credentials: "same-origin"/);
  assert.match(source, /response\.status === 404/);
  assert.match(source, /<MetricStrip items=\{metricItems\} label="Analysis health metrics"/);
  assert.match(source, /rankAnalysisHealth\(data\?\.traces \?\? \[\]/);
  assert.match(source, /problemSeverities\.map\(/);
  assert.match(source, /showHealthy/);
  assert.match(source, /parseTestDisplayName\(trace\.test_name\)/);
  assert.match(source, /shortJobName\(trace\.job_id, prefix\)/);
  assert.match(source, /testRunPath\(trace\.job_id, trace\.test_name, trace\.build_id\)/);
  assert.match(filters, /useState\(activeCount > 0\)/);
  assert.match(filters, /aria-expanded=\{open\}/);
  assert.match(filters, /minHeight: 48/);
  assert.match(ledger, /gridTemplateAreas:[\s\S]*"sequence kind" "elapsed outcome" "details details"/);
  assert.match(ledger, /minHeight: \{ xs: 44, md: 72 \}/);
  assert.match(filters + ledger, /prefers-reduced-motion: reduce/);
  assert.doesNotMatch(source + filters + ledger, /<Panel/);
  assert.doesNotMatch(source + filters + ledger, /<Accordion/);
  assert.doesNotMatch(source + filters + ledger, /<Chip/);
  assert.doesNotMatch(source + filters + ledger, /soft\(/);
  assert.doesNotMatch(source + filters + ledger, /borderRadius: "10px/);
});

test("inline trace inspector stays admin-gated and fetches only the selected analysis", () => {
  const source = readFileSync(
    resolve(process.cwd(), "src/components/AnalysisTraceInspector.tsx"),
    "utf8",
  );

  assert.match(source, /Boolean\(features\.analysis_health\) && auth\.status === "authenticated"/);
  assert.match(source, /if \(!enabled\) return null;/);
  assert.match(source, /job_id: reference\.job_id/);
  assert.match(source, /build_id: reference\.build_id/);
  assert.match(source, /test_name: reference\.test_name/);
  assert.match(source, /api\/analysis-health\?\$\{query\}/);
  assert.match(source, /credentials: "same-origin"/);
  assert.match(source, /response\.status === 404/);
  assert.match(source, /controllerRef\.current\?\.abort\(\)/);
  assert.match(source, /controller\.signal\.aborted/);
  assert.match(source, /state\.status !== "loaded"/);
  assert.match(source, /newestFirst\(state\.traces\)/);
});
