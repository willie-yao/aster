import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import * as ts from "typescript";
import { normalizeFlakinessReport } from "../src/lib/flakinessReport.js";

const source = readFileSync(resolve(process.cwd(), "src/pages/FlakinessPage.tsx"), "utf8");
const sourceFile = ts.createSourceFile(
  "FlakinessPage.tsx",
  source,
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TSX,
);

function tagName(node: ts.JsxOpeningLikeElement): string {
  return node.tagName.getText(sourceFile);
}

function interactiveKind(node: ts.JsxOpeningLikeElement): "button" | "link" | null {
  const name = tagName(node);
  if (
    name === "IconButton" ||
    name === "ButtonBase" ||
    name === "Button" ||
    name === "AccordionSummary" ||
    name === "button"
  ) {
    return "button";
  }
  if (name === "Link" || name === "RouterLink" || name === "a") {
    return "link";
  }

  const component = node.attributes.properties.find(
    (property): property is ts.JsxAttribute =>
      ts.isJsxAttribute(property) && property.name.getText(sourceFile) === "component",
  );
  const renderedComponent = component?.initializer?.getText(sourceFile) ?? "";
  if (renderedComponent === '"button"' || renderedComponent === "{Button}") return "button";
  if (renderedComponent === '"a"' || renderedComponent === "{RouterLink}") return "link";
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

  visit(sourceFile, []);
  return violations;
}

test("test and job links stay separate from the disclosure button", () => {
  assert.deepEqual(collectInteractiveNesting(), []);
  assert.doesNotMatch(source, /AccordionSummary/);
  assert.match(source, /testRunPath\(item\.job_id, item\.test_name, item\.last_failure\.build_id\)/);
  assert.match(source, /testPath\(item\.job_id, item\.test_name\)/);
  assert.match(source, /jobPath\(item\.job_id\)/);
  assert.match(
    source,
    /<ButtonBase[\s\S]*aria-controls=\{detailsId\}[\s\S]*aria-expanded=\{expanded\}/,
  );
  assert.match(source, /<Collapse in=\{expanded\} timeout="auto">/);
  assert.doesNotMatch(source, /unmountOnExit/);
});

test("mobile rows stack identity metrics and disclosure without clipping", () => {
  assert.match(source, /xs: '"primary details" "metrics metrics"'/);
  assert.match(source, /display: \{ xs: "grid", md: "contents" \}/);
  assert.match(
    source,
    /gridTemplateColumns: \{ xs: "repeat\(3, minmax\(0, 1fr\)\)" \}/,
  );
  assert.match(source, /minHeight: 44/);
});

test("focusable tabs own their visible names and descriptions", () => {
  assert.match(source, /label: "Flakiest tests"/);
  assert.match(source, /label: "Persistent failures"/);
  assert.match(source, /label: "Recent failures"/);
  assert.match(source, /label: "Build failures"/);
  assert.match(source, /at least \$\{persistentAfter\} consecutive failures/);
  assert.match(source, /within 48 hours of this published snapshot/);
  assert.match(source, /classification === "one-off"\) return "New failure streak"/);
  assert.doesNotMatch(source, /same error/);
  assert.doesNotMatch(source, /consistently broken/);
  assert.doesNotMatch(source, /new regressions/i);
  assert.match(
    source,
    /aria-describedby={`failure-trends-\$\{tab\.value\}-description`}/,
  );
  assert.match(
    source,
    /label=\{<TabLabel label=\{tab\.label\} count=\{tabCounts\[tab\.value\]\} \/>\}/,
  );
  assert.match(source, /title=\{tab\.tooltip\}/);
  assert.match(source, /height: "1px"[\s\S]*width: "1px"/);
  assert.doesNotMatch(source, /<Tooltip/);
  assert.doesNotMatch(source, /<Tab(?=[\s>])[^>]*aria-label=/);
});


test("published freshness stays separate from background refresh progress", () => {
  assert.match(source, />\s*Published results\s*</);
  assert.match(source, /refreshPresentation\?\.title \?\? "Refresh in progress"/);
  assert.match(source, /Published results remain available until the refresh completes\./);
  assert.match(source, /Showing the last published build failures\. A new snapshot is currently being prepared\./);
  assert.match(source, /fetchStatus\?\.state === "active"/);
  assert.match(source, /refreshPresentation\?\.detail \?\? "Preparing the next published snapshot"/);
  assert.match(source, /fetchStatusPresentation\(fetchStatus\)/);
  assert.doesNotMatch(source, /aria-live=/);
});


test("build failures use a bounded summary surface and canonical links", () => {
  assert.match(source, />\s*Failure Trends\s*</);
  assert.match(source, /function BuildFailureRow/);
  assert.match(source, /to={item\.job_detail_url}/);
  assert.match(source, /aria-label={`Open details for \$\{item\.job_name\} build \$\{item\.build_id\}`}/);
  assert.match(source, /aria-describedby=\{summaryId\}/);
  assert.match(source, /<Typography id=\{summaryId\}[\s\S]*\{summary\}/);
  assert.match(source, /item\.build_log_url/);
  assert.match(source, /item\.summary \|\| "No accepted build analysis is available for this run\."/);
  assert.match(source, /item\.provenance === "cache"/);
  assert.doesNotMatch(source, /item\.root_cause/);
  assert.doesNotMatch(source, /item\.suggested_fix/);
});

test("failure trends use continuous operator-console structure", () => {
  assert.match(source, /<DetailSectionBand/);
  assert.match(source, /borderRadius: 0/);
  assert.match(
    source,
    /boxShadow: "inset 0 -3px 0 var\(--mui-palette-primary-main\)"/,
  );
  assert.match(
    source,
    /borderTopColor: "var\(--mui-palette-divider\)"/,
  );
  assert.match(
    source,
    /fontSize: \{ xs: "26px", sm: "30px" \}/,
  );
  assert.doesNotMatch(source, /<Panel/);
  assert.doesNotMatch(source, /<Chip/);
  assert.doesNotMatch(source, /<LinearProgress/);
  assert.doesNotMatch(source, /borderRadius: 999/);
  assert.doesNotMatch(source, /0 0 8px/);
});

test("nullable production collections normalize before page rendering", () => {
  const report = normalizeFlakinessReport({
    generated_at: "2026-08-06T08:26:27Z",
    most_flaky: null,
    persistent_failures: null,
    recently_broken: null,
    build_failures: null,
    recurring_patterns: null,
  });

  assert.deepEqual(report.most_flaky, []);
  assert.deepEqual(report.persistent_failures, []);
  assert.deepEqual(report.recently_broken, []);
  assert.deepEqual(report.build_failures, []);
  assert.deepEqual(report.recurring_patterns, []);
});

test("layout contains route rendering failures without removing shared navigation", () => {
  const layout = readFileSync(resolve(process.cwd(), "src/components/Layout.tsx"), "utf8");
  const boundary = readFileSync(resolve(process.cwd(), "src/components/RouteErrorBoundary.tsx"), "utf8");
  const dataHook = readFileSync(resolve(process.cwd(), "src/hooks/useData.ts"), "utf8");

  assert.match(dataHook, /normalizeFlakinessReport\(result\.data\)/);
  assert.match(layout, /<RouteErrorBoundary[\s\S]*<Outlet \/>[\s\S]*<\/RouteErrorBoundary>/);
  assert.match(boundary, /role="alert"/);
  assert.match(boundary, /title="Page unavailable"/);
  assert.match(boundary, /previous\.resetKey !== this\.props\.resetKey/);
});
