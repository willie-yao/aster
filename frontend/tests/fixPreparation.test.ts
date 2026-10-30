import assert from "node:assert/strict";
import test from "node:test";
import { ThemeProvider, type Theme } from "@mui/material/styles";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { withSSR } from "./helpers/ssr.js";

interface PreparationProps {
  context: ReactNode;
  controls: ReactNode;
  notices: ReactNode;
  actions: ReactNode;
  closeDisabled: boolean;
  onClose: () => void;
}

const { FixPreparationWorkspace, defaultTheme } = await withSSR(async (vite) => {
  const { FixPreparationWorkspace } = (await vite.ssrLoadModule("/src/components/FixPreparationWorkspace.tsx")) as {
    FixPreparationWorkspace: (props: PreparationProps) => ReturnType<typeof createElement>;
  };
  const { defaultTheme } = (await vite.ssrLoadModule("/src/theme/index.ts")) as { defaultTheme: Theme };
  return { FixPreparationWorkspace, defaultTheme };
});

function render(overrides: Partial<PreparationProps> = {}): string {
  const props: PreparationProps = {
    context: createElement("p", null, "The selected finding and its evidence"),
    controls: createElement("textarea", { "aria-label": "Maintainer instruction", defaultValue: "Keep the patch narrow." }),
    notices: null,
    actions: createElement("button", null, "Generate fix preview"),
    closeDisabled: false,
    onClose: () => {},
    ...overrides,
  };
  return renderToStaticMarkup(createElement(ThemeProvider, { theme: defaultTheme }, createElement(FixPreparationWorkspace, props)))
    .replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, "");
}

test("fix preparation separates the finding from its controls and keeps generation actions last", () => {
  const html = render();
  assert.match(html, /<h2[^>]*>Use this finding in a fix proposal<\/h2>/);
  assert.doesNotMatch(html, /Review context and instructions before generating the preview/);
  assert.match(html, /<section[^>]*aria-label="Fix context"[^>]*>[\s\S]*?The selected finding and its evidence[\s\S]*?<\/section>/);
  assert.match(html, /<section[^>]*aria-label="Prepare the fix"[^>]*>[\s\S]*?<textarea[^>]*aria-label="Maintainer instruction"[^>]*>Keep the patch narrow\.<\/textarea>[\s\S]*?<\/section>/);
  assert.ok(html.indexOf("Generate fix preview") > html.indexOf("Maintainer instruction"));
  assert.match(html, /<button[^>]*aria-label="Close"/);
  assert.doesNotMatch(html, /<button[^>]*disabled/);
});

test("fix preparation preserves progress notices and the close guard", () => {
  const html = render({
    notices: createElement("p", { role: "status" }, "The request could not be refreshed."),
    controls: createElement("p", null, "Maintainer instruction: Keep the patch narrow."),
    actions: createElement("p", { role: "status" }, "Generating the fix preview"),
    closeDisabled: true,
  });
  assert.match(html, /role="status">The request could not be refreshed\./);
  assert.match(html, /Maintainer instruction: Keep the patch narrow\./);
  assert.match(html, /role="status">Generating the fix preview/);
  assert.match(html, /<button[^>]*disabled=""[^>]*aria-label="Close"/);
});

test("completed fix preparation shows its result without stale context or instructions", () => {
  const html = render({
    context: null,
    notices: createElement("a", { href: "https://github.com/example/repo/pull/1" }, "Draft PR opened"),
    actions: createElement("button", null, "Done"),
  });
  assert.match(html, /href="https:\/\/github\.com\/example\/repo\/pull\/1">Draft PR opened/);
  assert.match(html, /<button>Done<\/button>/);
  assert.doesNotMatch(html, /aria-label="Fix context"|aria-label="Prepare the fix"|<textarea/);
});
