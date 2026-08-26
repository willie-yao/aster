import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
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
const overviewModule = (await vite.ssrLoadModule("/src/theme/overview.ts")) as {
  overviewTypography: Record<string, Record<string, string | number>>;
  overviewLayout: Record<string, string | number>;
};
await vite.close();

const { defaultTheme, darkTokens, lightTokens } = themeModule;
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

// A tinted status chip lifts its own background toward its accent, which is
// what pushed `main` under 4.5:1 in light mode. Both schemes are checked here
// because the failure only appeared in one of them.
test("tinted status chips keep their label readable in both schemes", () => {
  const hex = (h: string) => ({
    r: parseInt(h.slice(1, 3), 16),
    g: parseInt(h.slice(3, 5), 16),
    b: parseInt(h.slice(5, 7), 16),
  });
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

  const schemes = {
    light: { tokens: lightTokens, label: (accent: string) => lightTokens[`${accent}Dim`] },
    dark: { tokens: darkTokens, label: (accent: string) => darkTokens[accent] },
  };
  const accents = { primary: "primary", success: "secondary", warning: "tertiary" } as const;

  for (const [name, scheme] of Object.entries(schemes)) {
    for (const [chipColor, tokenKey] of Object.entries(accents)) {
      const accent = hex(scheme.tokens[tokenKey]);
      // The chip sits on the assistant message, itself tinted at 0.045.
      const bubble = over(accent, 0.045, hex(scheme.tokens.surfaceContainer));
      const chipBackground = over(accent, 0.16, bubble);
      const ratio = contrast(hex(scheme.label(tokenKey)), chipBackground);
      assert.ok(
        ratio >= 4.5,
        `${name} ${chipColor} chip label contrast ${ratio.toFixed(2)} is below 4.5:1`,
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
