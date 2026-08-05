import assert from "node:assert/strict";
import { test } from "node:test";
import type { Theme } from "@mui/material/styles";
import { createServer } from "vite";

const vite = await createServer({
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: "custom",
  logLevel: "silent",
  ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
});
const themeModule = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
  defaultTheme: Theme;
  darkTokens: Record<string, string>;
  lightTokens: Record<string, string>;
};
await vite.close();

const { defaultTheme, darkTokens, lightTokens } = themeModule;

test("operator console palette uses graphite surfaces and Azure blue", () => {
  assert.equal(darkTokens.background, "#0d1117");
  assert.equal(darkTokens.surfaceContainer, "#161b22");
  assert.equal(darkTokens.outlineVariant, "#30363d");
  assert.equal(darkTokens.primary, "#2f81f7");
  assert.equal(darkTokens.onPrimary, "#0d1117");
  assert.equal(darkTokens.onSecondary, "#0d1117");
  assert.equal(lightTokens.background, "#f6f8fa");
  assert.equal(lightTokens.surfaceContainer, "#ffffff");
  assert.equal(lightTokens.outlineVariant, "#d0d7de");
  assert.equal(lightTokens.primary, "#0969da");
});

test("operator console theme keeps compact technical typography", () => {
  assert.equal(defaultTheme.shape.borderRadius, 6);
  const data = (defaultTheme.typography as unknown as { data: { fontFamily?: string } }).data;
  assert.match(String(data.fontFamily), /ui-monospace/);
  assert.equal(defaultTheme.typography.h4.fontSize, "1.5rem");
  assert.equal(defaultTheme.typography.body1.fontSize, "0.875rem");
});
