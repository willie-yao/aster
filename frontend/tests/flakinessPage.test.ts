import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import * as ts from "typescript";

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
  if (name === "IconButton" || name === "Button" || name === "AccordionSummary" || name === "button") {
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
  assert.match(source, /<IconButton[\s\S]*aria-controls=\{detailsId\}[\s\S]*aria-expanded=\{expanded\}/);
  assert.match(source, /<Collapse in=\{expanded\} timeout="auto">/);
  assert.doesNotMatch(source, /unmountOnExit/);
});

test("mobile rows reserve the first line for test and job names", () => {
  assert.match(source, /flex: \{ xs: "1 1 100%", sm: "1 1 auto" \}/);
  assert.match(source, /width: \{ xs: "100%", sm: "auto" \}/);
  assert.match(source, /flexWrap: \{ xs: "wrap", sm: "nowrap" \}/);
});

test("focusable tabs own their visible names and descriptions", () => {
  assert.match(source, /label: "Most Flaky"/);
  assert.match(source, /label: "Persistent Failures"/);
  assert.match(source, /label: "Recently Broken"/);
  assert.match(source, /label: "Build Failures"/);
  assert.match(source, /aria-describedby={`failure-analysis-\$\{t\.value\}-description`}/);
  assert.match(source, /label={`\$\{t\.label\} \$\{tabCounts\[t\.value\]}`}/);
  assert.match(source, /title=\{t\.tooltip\}/);
  assert.match(source, /height: "1px"[\s\S]*width: "1px"/);
  assert.doesNotMatch(source, /<Tooltip/);
  assert.doesNotMatch(source, /<Tab(?=[\s>])[^>]*aria-label=/);
});


test("published freshness stays separate from background refresh progress", () => {
  assert.match(source, />\s*Published results\s*</);
  assert.match(source, />\s*Refresh in progress\s*</);
  assert.match(source, /Published results remain available until the refresh completes\./);
  assert.match(source, /Showing the last published build failures\. A new snapshot is currently being prepared\./);
  assert.match(source, /fetchStatus\?\.state === "active"/);
  assert.match(source, /refreshProgress\.ready} of \$\{refreshProgress\.total} results ready/);
  assert.doesNotMatch(source, /aria-live=/);
});


test("build failures use a bounded summary surface and canonical links", () => {
  assert.match(source, />\s*Failure Analysis\s*</);
  assert.match(source, /function BuildFailureRow/);
  assert.match(source, /to={item\.job_detail_url}/);
  assert.match(source, /aria-label={`Open details for \$\{item\.job_name\} build \$\{item\.build_id\}`}/);
  assert.match(source, /item\.build_log_url/);
  assert.match(source, /item\.summary \|\| "No accepted build analysis is available for this run\."/);
  assert.match(source, /item\.provenance === "cache"/);
  assert.doesNotMatch(source, /item\.root_cause/);
  assert.doesNotMatch(source, /item\.suggested_fix/);
});
