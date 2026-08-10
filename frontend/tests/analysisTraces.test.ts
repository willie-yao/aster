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
const { AnalysisTraceLedger, TraceEventRow } = (await vite.ssrLoadModule(
  "/src/components/AnalysisTraceLedger.tsx",
)) as {
  AnalysisTraceLedger: (props: { items: Array<{
    trace: AnalysisTrace;
    displayTitle: string;
    displayJob: string;
    testHref: string;
    responseIDs: string[];
  }> }) => ReturnType<typeof createElement>;
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
    outcome: "succeeded",
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
  assert.equal(traceStatusLabel("ai_cache_hit"), "AI cache hit");
  assert.equal(traceTone("ai_cache_hit"), "success");
  assert.equal(traceTone("retry"), "warning");
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
    "quality_floor_exhausted",
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

test("trace ledger uses full-row disclosures and separate route and copy actions", () => {
  const item = {
    trace: trace(),
    displayTitle: "Highly-available cluster",
    displayJob: "e2e-main",
    testHref: "/job/job-main/test/test-name?run=20134789654",
    responseIDs: ["resp-one", "resp-two"],
  };
  const html = render(createElement(AnalysisTraceLedger, { items: [item] }));

  assert.match(html, /<h2[^>]*>Trace ledger<\/h2>/);
  assert.match(html, /aria-expanded="false"/);
  assert.match(html, /aria-controls="analysis-trace-/);
  assert.match(html, /aria-label="Expand Succeeded trace for Highly-available cluster\./);
  assert.match(html, /Job e2e-main\. Build 20134789654\. API mode responses\. 84\.2 s\. 3 events\./);
  assert.match(html, /title="\[It\] Workload cluster creation Creating a highly-available cluster"/);
  assert.match(html, /title="periodic-cluster-api-provider-azure-e2e-main"/);
  assert.match(html, /href="\/job\/job-main\/test\/test-name\?run=20134789654"/);
  assert.match(html, /Copy build 20134789654/);
  assert.match(html, /Copy response 1 resp-one/);
  assert.match(html, /Copy response 2 resp-two/);
  assert.deepEqual(collectInteractiveNesting(), []);
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

test("Analysis Traces page preserves private gates query downloads and operator-console structure", () => {
  const source = readFileSync(resolve(process.cwd(), "src/pages/AnalysisTracesPage.tsx"), "utf8");
  const filters = readFileSync(resolve(process.cwd(), "src/components/AnalysisTraceFilters.tsx"), "utf8");
  const ledger = readFileSync(resolve(process.cwd(), "src/components/AnalysisTraceLedger.tsx"), "utf8");

  assert.match(source, /if \(!features\.analysis_traces\)/);
  assert.match(source, /if \(auth\.status === "loading"\)/);
  assert.match(source, /if \(auth\.status === "anonymous"\)/);
  assert.match(source, /api\/analysis-traces\$\{query \? `\?\$\{query\}` : ""\}/);
  assert.match(source, /api\/analysis-traces\/download\$\{query \? `\?\$\{query\}` : ""\}/);
  assert.match(source, /credentials: "same-origin"/);
  assert.match(source, /response\.status === 404/);
  assert.match(source, /<MetricStrip items=\{metricItems\} label="Trace metrics"/);
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
