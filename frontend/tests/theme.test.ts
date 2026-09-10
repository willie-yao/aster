import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { createTheme, type SxProps, type Theme } from "@mui/material/styles";
import { withSSR } from "./helpers/ssr.js";
import * as ts from "typescript";

type Accent = "primary" | "success" | "warning";

const { themeModule, overviewModule } = await withSSR(async (vite) => {
  const themeModule = (await vite.ssrLoadModule("/src/theme/index.ts")) as {
    defaultTheme: Theme & { colorSchemes: Record<"light" | "dark", { palette: Theme["palette"] }> };
    darkTokens: Record<string, string>;
    lightTokens: Record<string, string>;
    soft: (theme: Theme, color: Accent, opacity: number) => string;
    accentLabelSx: (theme: Theme, color: Accent) => SxProps<Theme>;
    softChipSx: (theme: Theme, color: Accent) => SxProps<Theme>;
  };
  const overviewModule = (await vite.ssrLoadModule("/src/theme/overview.ts")) as {
    overviewTypography: Record<string, Record<string, string | number>>;
    overviewLayout: Record<string, string | number>;
  };
  return { themeModule, overviewModule };
});

const { defaultTheme, darkTokens, lightTokens, soft, accentLabelSx, softChipSx } = themeModule;
const { overviewTypography, overviewLayout } = overviewModule;

test("operator console palette uses graphite surfaces and brand violet", () => {
  assert.equal(darkTokens.background, "#0d1117");
  assert.equal(darkTokens.surfaceContainer, "#161b22");
  assert.equal(darkTokens.outlineVariant, "#30363d");
  assert.equal(darkTokens.primary, "#a78bfa");
  assert.equal(darkTokens.primaryDim, "#c4b5fd");
  assert.equal(darkTokens.tertiaryDim, "#a9791b");
  assert.equal(darkTokens.onPrimary, "#0d1117");
  assert.equal(darkTokens.onSecondary, "#0d1117");
  assert.equal(lightTokens.background, "#f6f8fa");
  assert.equal(lightTokens.surfaceContainer, "#ffffff");
  assert.equal(lightTokens.outlineVariant, "#d0d7de");
  assert.equal(lightTokens.primary, "#7c3aed");
  assert.equal(lightTokens.tertiaryDim, "#7d4e00");
});

test("brand gradient stops are exposed and distinct from status colors", () => {
  assert.equal(lightTokens.brandFrom, "#7c3aed");
  assert.equal(lightTokens.brandTo, "#ec4899");
  assert.equal(darkTokens.brandFrom, "#a78bfa");
  assert.equal(darkTokens.brandTo, "#f472b6");
  // palette.brand is a module augmentation; this config does not compile the
  // theme sources, so read it the same way the typography variants are read.
  const palette = defaultTheme.palette as unknown as {
    brand: { from: string; to: string };
  };
  assert.equal(palette.brand.from, darkTokens.brandFrom);
  assert.equal(palette.brand.to, darkTokens.brandTo);
  // Status colors carry CI meaning and must not be restyled to the brand.
  assert.equal(lightTokens.dotPass, "#1a7f37");
  assert.equal(lightTokens.dotFail, "#cf222e");
  assert.equal(darkTokens.dotPass, "#3fb950");
  assert.equal(darkTokens.dotFail, "#f85149");
});

test("operator console theme keeps compact technical typography", () => {
  // One radius for the whole app. Surfaces that need a squarer or rounder
  // corner say so locally; nothing has to override the default back to 4px.
  assert.equal(defaultTheme.shape.borderRadius, 4);
  const data = (defaultTheme.typography as unknown as { data: { fontFamily?: string } }).data;
  assert.match(String(data.fontFamily), /ui-monospace/);
  assert.equal(defaultTheme.typography.h4.fontSize, "1.75rem");
  assert.equal(defaultTheme.typography.body1.fontSize, "1rem");
  assert.equal(defaultTheme.typography.body2.fontSize, "0.875rem");
  const customTypography = defaultTheme.typography as unknown as { data: { fontSize?: string }; stat: { fontSize?: string } };
  assert.equal(customTypography.data.fontSize, "0.8125rem");
  assert.equal(customTypography.stat.fontSize, "1.75rem");
  assert.match(readFileSync(resolve(process.cwd(), "src/index.css"), "utf8"), /font-size: 17px/);
});


test("overview typography uses the approved compact readable scale", () => {
  assert.equal(overviewTypography.pageHeadline.fontSize, "27px");
  assert.equal(overviewTypography.majorHeading.fontSize, "18px");
  assert.equal(overviewTypography.categoryHeading.fontSize, "16px");
  assert.equal(overviewTypography.subsectionHeading.fontSize, "13.5px");
  assert.equal(overviewTypography.primaryBody.fontSize, "15px");
  assert.equal(overviewTypography.mobileFeaturedBody.fontSize, "16px");
  assert.equal(overviewTypography.jobIdentifier.fontSize, "14px");
  assert.equal(overviewTypography.data.fontSize, "13px");
  assert.equal(overviewTypography.tableHeading.fontSize, "13px");
  assert.equal(overviewLayout.majorBandMinHeight, 48);
  assert.equal(overviewLayout.categoryBandMinHeight, 44);
  assert.equal(overviewLayout.ledgerRowMinHeight, 52);
});

test("buttons inherit the theme font instead of the user agent's", () => {
  // ButtonBase sets no font of its own, so without this a bare button falls
  // back to Arial. It showed up on the metric strip, the disclosure buttons,
  // the result filters, and the rail's search label.
  const root = defaultTheme.components?.MuiButtonBase?.styleOverrides?.root as Record<string, unknown>;
  assert.equal(root.font, "inherit");
  // The ripple is decorative interaction motion, so it goes when motion is
  // reduced; press still reads through the surface change.
  assert.deepEqual(root["@media (prefers-reduced-motion: reduce)"], {
    "& .MuiTouchRipple-root": { display: "none" },
  });
});

// Chip is the one MUI primitive that hardcodes a pill rather than reading
// shape.borderRadius, so the whole app renders pills if this override is lost.
test("chips square to the shape token instead of rendering as pills", () => {
  const root = defaultTheme.components?.MuiChip?.styleOverrides?.root;
  assert.equal(typeof root, "function");
  const resolved = (root as (p: { theme: Theme }) => { borderRadius: number })({ theme: defaultTheme });
  assert.equal(resolved.borderRadius, defaultTheme.shape.borderRadius);
});

function assistantTintOpacity(): number {
  const file = ts.createSourceFile(
    "AnalysisChat.tsx", readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8"),
    ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX,
  );
  const assistant = file.statements.find((node): node is ts.FunctionDeclaration =>
    ts.isFunctionDeclaration(node) && node.name?.text === "AssistantMessage");
  assert.ok(assistant?.body, "missing AssistantMessage");
  const returned = assistant.body.statements.find(ts.isReturnStatement)?.expression;
  assert.ok(returned && ts.isParenthesizedExpression(returned) && ts.isJsxElement(returned.expression));
  const box = returned.expression.openingElement;
  assert.equal(box.tagName.getText(file), "Box");
  const sx = box.attributes.properties.filter(ts.isJsxAttribute)
    .find((attribute) => attribute.name.getText(file) === "sx")?.initializer;
  assert.ok(sx && ts.isJsxExpression(sx) && sx.expression && ts.isObjectLiteralExpression(sx.expression));
  const background = sx.expression.properties.filter(ts.isPropertyAssignment)
    .find((property) => property.name.getText(file) === "bgcolor")?.initializer;
  assert.ok(background && ts.isArrowFunction(background) && ts.isCallExpression(background.body));
  const tint = background.body;
  assert.equal(tint.expression.getText(file), "soft");
  assert.deepEqual(tint.arguments.slice(0, 2).map((argument) => argument.getText(file)), ["theme", "accent"]);
  const opacity = tint.arguments[2];
  assert.ok(opacity && ts.isConditionalExpression(opacity));
  assert.equal(opacity.condition.getText(file), "evidenceWarning");
  assert.ok(ts.isNumericLiteral(opacity.whenFalse), "missing normal assistant tint opacity");

  const chipCalls: ts.CallExpression[] = [];
  const header = returned.expression.children.find((node): node is ts.JsxElement =>
    ts.isJsxElement(node) && node.openingElement.tagName.getText(file) === "Stack");
  assert.ok(header, "missing assistant header");
  const chip = header.children.find((node): node is ts.JsxSelfClosingElement =>
    ts.isJsxSelfClosingElement(node) && node.tagName.getText(file) === "Chip");
  assert.ok(chip, "missing assistant verdict chip");
  const chipSx = chip.attributes.properties.filter(ts.isJsxAttribute)
    .find((attribute) => attribute.name.getText(file) === "sx");
  assert.ok(chipSx, "missing verdict styles");
  function visit(node: ts.Node) {
    if (ts.isCallExpression(node) && node.expression.getText(file) === "softChipSx") chipCalls.push(node);
    ts.forEachChild(node, visit);
  }
  visit(chipSx);
  assert.equal(chipCalls.length, 1, "assistant verdict must use the tested chip helper");
  assert.ok(ts.isSpreadAssignment(chipCalls[0].parent));
  assert.deepEqual(chipCalls[0].arguments.map((argument) => argument.getText(file)), ["theme", "accent"]);
  return Number(opacity.whenFalse.text);
}

test("tinted status chips keep their label readable in both schemes", () => {
  const hex = (h: string) => {
    assert.match(h, /^#[0-9a-f]{6}$/iu);
    return {
      r: parseInt(h.slice(1, 3), 16),
      g: parseInt(h.slice(3, 5), 16),
      b: parseInt(h.slice(5, 7), 16),
    };
  };
  type RGB = ReturnType<typeof hex>;
  const over = (fg: RGB, a: number, bg: RGB): RGB => ({
    r: fg.r * a + bg.r * (1 - a),
    g: fg.g * a + bg.g * (1 - a),
    b: fg.b * a + bg.b * (1 - a),
  });
  const luminance = (c: RGB) => {
    const f = (v: number) => {
      const s = v / 255;
      return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };
  const contrast = (a: RGB, b: RGB) => {
    const [l1, l2] = [luminance(a), luminance(b)];
    return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
  };

  const rgba = (value: string) => {
    const match = /^rgba\(([\d.]+),\s*([\d.]+),\s*([\d.]+),\s*([\d.]+)\)$/u.exec(value);
    assert.ok(match, `expected helper RGBA output, got ${value}`);
    const [, r, g, b, alpha] = match.map(Number);
    assert.ok(alpha >= 0 && alpha <= 1);
    return { color: { r, g, b }, alpha };
  };
  const bubbleOpacity = assistantTintOpacity();
  for (const mode of ["light", "dark"] as const) {
    const palette = defaultTheme.colorSchemes?.[mode]?.palette;
    assert.ok(palette, `missing ${mode} palette`);
    // MUI resolves palette paths and applyStyles for each concrete scheme.
    const theme = createTheme({ palette: { ...palette, mode } });
    for (const accent of ["primary", "success", "warning"] as const) {
      const chip = theme.unstable_sx(softChipSx(theme, accent)) as { color: string; backgroundColor: string };
      const label = theme.unstable_sx(accentLabelSx(theme, accent)) as { color: string };
      assert.equal(chip.color, label.color);
      const bubbleTint = rgba(soft(theme, accent, bubbleOpacity));
      const chipTint = rgba(chip.backgroundColor);
      const bubble = over(bubbleTint.color, bubbleTint.alpha, hex(theme.palette.background.paper));
      const chipBackground = over(chipTint.color, chipTint.alpha, bubble);
      const ratio = contrast(hex(chip.color), chipBackground);
      assert.ok(
        ratio >= 4.5,
        `${mode} ${accent} chip label contrast ${ratio.toFixed(2)} is below 4.5:1`,
      );
    }
  }
});

// Reduced motion neutralizes transitions but deliberately not animation. A
// blanket animation rule runs a progress spinner exactly once and leaves it
// frozen, which reports nothing; looping motion is stopped per element instead.
test("reduced motion keeps progress feedback and stops decorative loops", () => {
  const css = readFileSync(resolve(process.cwd(), "src/index.css"), "utf8");
  const chat = readFileSync(resolve(process.cwd(), "src/components/AnalysisChat.tsx"), "utf8");
  const globalRule = /@media \(prefers-reduced-motion: reduce\) \{\s*\*,[\s\S]*?\n\}/.exec(css)?.[0] ?? "";

  assert.match(globalRule, /transition-duration: 0\.01ms !important/);
  assert.doesNotMatch(globalRule, /animation-duration/);
  assert.doesNotMatch(globalRule, /animation-iteration-count/);

  // The two looping decorations carry nothing the interface would lose.
  assert.match(css, /prefers-reduced-motion: reduce\) \{\s*\.ai-aurora::before \{\s*animation: none/);
  assert.match(chat, /analysisChatPulse[\s\S]{0,260}?"@media \(prefers-reduced-motion: reduce\)": \{ animation: "none" \}/);
  // An explicit scroll behavior overrides the CSS rule, so the preference has
  // to be read at the call site.
  assert.match(chat, /behavior: reducedMotion \? "auto" : "smooth"/);
});
